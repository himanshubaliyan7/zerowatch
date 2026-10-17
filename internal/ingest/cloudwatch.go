package ingest

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/commitlog"
	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
	"github.com/himanshubaliyan7/zerowatch/internal/query"
)

// CloudWatch compatibility layer. Supports both wire protocols the AWS SDKs
// use for these APIs:
//
//   - awsQuery (form-encoded, XML responses): Action=PutMetricData&Namespace=..&MetricData.member.1.MetricName=..
//   - awsJson1.0/1.1 (X-Amz-Target header, JSON bodies):
//     GraniteServiceVersion20100801.PutMetricData / .GetMetricData
//     Logs_20140328.PutLogEvents / .CreateLogGroup / .CreateLogStream
//
// Point an SDK at ZeroWatch with endpoint_url=http://host:8080/cloudwatch.
// With --aws-credentials (ZW_AWS_CREDENTIALS=AKID:SECRET:tenant) SigV4
// signatures are fully verified (services monitoring/logs, any region, ±15m
// skew; see httpx/sigv4.go). Without it the access key id is mapped through
// --api-keys and the signature is NOT verified. The newer smithy-rpcv2-cbor
// protocol is not supported.

const cwXMLNS = "http://monitoring.amazonaws.com/doc/2010-08-01/"

// MaxCWBody bounds CloudWatch request bodies (PutMetricData limit is 1MB).
const MaxCWBody = 1 << 20

// CloudWatchHandler serves the compatibility endpoints.
type CloudWatchHandler struct {
	Pipeline *Pipeline
	Query    *query.Engine
}

func (h *CloudWatchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		awsError(w, r, false, http.StatusMethodNotAllowed, "InvalidAction", "POST required")
		return
	}
	tenant := httpx.Tenant(r.Context())
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		body, err := httpx.ReadBody(w, r, MaxCWBody)
		if err != nil {
			awsError(w, r, true, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge", err.Error())
			return
		}
		_, op, _ := strings.Cut(target, ".")
		h.serveJSON(w, r, tenant, op, body)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxCWBody)
	if err := r.ParseForm(); err != nil {
		awsError(w, r, false, http.StatusBadRequest, "MalformedQueryString", err.Error())
		return
	}
	switch r.PostForm.Get("Action") {
	case "PutMetricData":
		ms, err := ParsePutMetricDataForm(r.PostForm)
		if err != nil {
			awsError(w, r, false, http.StatusBadRequest, "InvalidParameterValue", err.Error())
			return
		}
		if !h.putMetrics(w, r, false, tenant, ms) {
			return
		}
		writeXML(w, "PutMetricDataResponse", httpx.RequestID(r.Context()))
	default:
		awsError(w, r, false, http.StatusBadRequest, "InvalidAction", "unsupported Action "+strconv.Quote(r.PostForm.Get("Action")))
	}
}

func (h *CloudWatchHandler) putMetrics(w http.ResponseWriter, r *http.Request, isJSON bool, tenant uint32, ms []model.MetricDatum) bool {
	if len(ms) == 0 || len(ms) > 1000 {
		awsError(w, r, isJSON, http.StatusBadRequest, "InvalidParameterValue", "MetricData must contain 1..1000 items")
		return false
	}
	res, err := h.Pipeline.WriteMetrics(r.Context(), tenant, ms)
	switch {
	case errors.Is(err, commitlog.ErrBackpressure):
		awsError(w, r, isJSON, http.StatusServiceUnavailable, "ServiceUnavailable", "ingest backpressure, retry")
		return false
	case err != nil:
		awsError(w, r, isJSON, http.StatusBadRequest, "InvalidParameterValue", err.Error())
		return false
	case res.Rejected > 0:
		// CloudWatch rejects the whole call on any invalid datum; we accept the
		// valid ones but still surface the error so SDK users notice.
		awsError(w, r, isJSON, http.StatusBadRequest, "InvalidParameterValue", strings.Join(res.Errors, "; "))
		return false
	}
	return true
}

// --- awsJson ---------------------------------------------------------------

type cwJSONDim struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type cwJSONDatum struct {
	MetricName        string              `json:"MetricName"`
	Dimensions        []cwJSONDim         `json:"Dimensions"`
	Timestamp         *float64            `json:"Timestamp"` // epoch seconds
	Value             *float64            `json:"Value"`
	Values            []float64           `json:"Values"`
	Counts            []float64           `json:"Counts"`
	StatisticValues   *model.StatisticSet `json:"StatisticValues"`
	Unit              string              `json:"Unit"`
	StorageResolution uint32              `json:"StorageResolution"`
}

type cwGetMetricDataReq struct {
	MetricDataQueries []struct {
		ID         string `json:"Id"`
		Label      string `json:"Label"`
		Expression string `json:"Expression"`
		ReturnData *bool  `json:"ReturnData"`
		Period     int    `json:"Period"`
		MetricStat *struct {
			Metric struct {
				Namespace  string      `json:"Namespace"`
				MetricName string      `json:"MetricName"`
				Dimensions []cwJSONDim `json:"Dimensions"`
			} `json:"Metric"`
			Period int    `json:"Period"`
			Stat   string `json:"Stat"`
		} `json:"MetricStat"`
	} `json:"MetricDataQueries"`
	StartTime float64 `json:"StartTime"`
	EndTime   float64 `json:"EndTime"`
	ScanBy    string  `json:"ScanBy"`
}

func (h *CloudWatchHandler) serveJSON(w http.ResponseWriter, r *http.Request, tenant uint32, op string, body []byte) {
	switch op {
	case "PutMetricData":
		var req struct {
			Namespace  string        `json:"Namespace"`
			MetricData []cwJSONDatum `json:"MetricData"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			awsError(w, r, true, 400, "SerializationException", err.Error())
			return
		}
		var ms []model.MetricDatum
		for _, d := range req.MetricData {
			ms = append(ms, jsonDatum(req.Namespace, d)...)
		}
		if h.putMetrics(w, r, true, tenant, ms) {
			writeAWSJSON(w, map[string]any{})
		}
	case "GetMetricData":
		h.getMetricData(w, r, tenant, body)
	case "PutLogEvents":
		var req struct {
			LogGroupName  string `json:"logGroupName"`
			LogStreamName string `json:"logStreamName"`
			LogEvents     []struct {
				Timestamp int64  `json:"timestamp"`
				Message   string `json:"message"`
			} `json:"logEvents"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			awsError(w, r, true, 400, "SerializationException", err.Error())
			return
		}
		if len(req.LogEvents) == 0 {
			awsError(w, r, true, 400, "InvalidParameterException", "logEvents must not be empty")
			return
		}
		evs := make([]model.LogEvent, len(req.LogEvents))
		for i, e := range req.LogEvents {
			evs[i] = model.LogEvent{LogGroup: req.LogGroupName, LogStream: req.LogStreamName, Timestamp: e.Timestamp, Message: e.Message}
		}
		res, err := h.Pipeline.WriteLogs(r.Context(), tenant, evs)
		if errors.Is(err, commitlog.ErrBackpressure) {
			awsError(w, r, true, 503, "ServiceUnavailableException", "ingest backpressure, retry")
			return
		}
		if err != nil {
			awsError(w, r, true, 400, "InvalidParameterException", err.Error())
			return
		}
		out := map[string]any{"nextSequenceToken": strconv.FormatInt(time.Now().UnixNano(), 10)}
		if res.Rejected > 0 {
			out["rejectedLogEventsInfo"] = map[string]any{"errors": res.Errors}
		}
		writeAWSJSON(w, out)
	case "CreateLogGroup", "CreateLogStream", "PutRetentionPolicy":
		// Groups/streams are created implicitly on first write.
		writeAWSJSON(w, map[string]any{})
	default:
		awsError(w, r, true, 400, "UnknownOperationException", "unsupported operation "+strconv.Quote(op))
	}
}

func jsonDatum(ns string, d cwJSONDatum) []model.MetricDatum {
	base := model.MetricDatum{Namespace: ns, MetricName: d.MetricName, Unit: d.Unit, StorageResolution: d.StorageResolution}
	if d.Timestamp != nil {
		base.Timestamp = int64(*d.Timestamp * 1000)
	}
	for _, dm := range d.Dimensions {
		base.Dimensions = append(base.Dimensions, model.Dimension{Name: dm.Name, Value: dm.Value})
	}
	switch {
	case d.StatisticValues != nil:
		base.StatisticValues = d.StatisticValues
		return []model.MetricDatum{base}
	case len(d.Values) > 0:
		return expandValues(base, d.Values, d.Counts)
	default:
		base.Value = d.Value
		return []model.MetricDatum{base}
	}
}

// expandValues folds the Values/Counts array form into one StatisticSet.
func expandValues(base model.MetricDatum, vals, counts []float64) []model.MetricDatum {
	s := model.StatisticSet{Minimum: math.Inf(1), Maximum: math.Inf(-1)}
	for i, v := range vals {
		c := 1.0
		if i < len(counts) {
			c = counts[i]
		}
		s.SampleCount += c
		s.Sum += v * c
		s.Minimum = math.Min(s.Minimum, v)
		s.Maximum = math.Max(s.Maximum, v)
	}
	base.StatisticValues = &s
	return []model.MetricDatum{base}
}

func (h *CloudWatchHandler) getMetricData(w http.ResponseWriter, r *http.Request, tenant uint32, body []byte) {
	if h.Query == nil {
		awsError(w, r, true, 503, "ServiceUnavailableException", "query role not enabled on this node")
		return
	}
	var in cwGetMetricDataReq
	if err := json.Unmarshal(body, &in); err != nil {
		awsError(w, r, true, 400, "SerializationException", err.Error())
		return
	}
	req := query.Request{TenantID: tenant, StartMs: int64(in.StartTime * 1000), EndMs: int64(in.EndTime * 1000), Fill: "null"}
	for _, q := range in.MetricDataQueries {
		mq := query.MetricDataQuery{ID: q.ID, Label: q.Label, Expression: q.Expression, ReturnData: q.ReturnData}
		if q.Period > 0 && req.Period == 0 {
			req.Period = q.Period
		}
		if ms := q.MetricStat; ms != nil {
			if req.Period == 0 {
				req.Period = ms.Period
			}
			dims := map[string]string{}
			for _, d := range ms.Metric.Dimensions {
				dims[d.Name] = d.Value
			}
			mq.MetricStat = &query.MetricStat{Namespace: ms.Metric.Namespace, MetricName: ms.Metric.MetricName, Dimensions: dims, Stat: ms.Stat}
		}
		req.Queries = append(req.Queries, mq)
	}
	if req.Period == 0 {
		req.Period = 60
	}
	results, err := h.Query.Execute(r.Context(), req)
	if err != nil {
		awsError(w, r, true, 400, "InvalidParameterValueException", err.Error())
		return
	}
	type cwResult struct {
		ID         string    `json:"Id"`
		Label      string    `json:"Label"`
		Timestamps []float64 `json:"Timestamps"`
		Values     []float64 `json:"Values"`
		StatusCode string    `json:"StatusCode"`
	}
	out := make([]cwResult, 0, len(results))
	for _, res := range results {
		cr := cwResult{ID: res.ID, Label: res.Label, StatusCode: res.StatusCode, Timestamps: []float64{}, Values: []float64{}}
		// CloudWatch omits empty periods and returns newest first (TimestampDescending default).
		for i := len(res.Timestamps) - 1; i >= 0; i-- {
			if math.IsNaN(res.Values[i]) {
				continue
			}
			cr.Timestamps = append(cr.Timestamps, float64(res.Timestamps[i])/1000)
			cr.Values = append(cr.Values, res.Values[i])
		}
		if in.ScanBy == "TimestampAscending" {
			reverse(cr.Timestamps)
			reverse(cr.Values)
		}
		out = append(out, cr)
	}
	writeAWSJSON(w, map[string]any{"MetricDataResults": out, "Messages": []any{}})
}

func reverse(s []float64) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// --- awsQuery form parsing ----------------------------------------------------

// ParsePutMetricDataForm parses the awsQuery encoding of PutMetricData.
func ParsePutMetricDataForm(f url.Values) ([]model.MetricDatum, error) {
	ns := f.Get("Namespace")
	type acc struct {
		d    model.MetricDatum
		dims map[int]*model.Dimension
		sv   *model.StatisticSet
		vals map[int]float64
		cnts map[int]float64
	}
	byIdx := map[int]*acc{}
	get := func(i int) *acc {
		a, ok := byIdx[i]
		if !ok {
			a = &acc{d: model.MetricDatum{Namespace: ns}, dims: map[int]*model.Dimension{}}
			byIdx[i] = a
		}
		return a
	}
	num := func(k, v string) (float64, error) {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: not a number", k)
		}
		return x, nil
	}
	for key, vs := range f {
		rest, ok := strings.CutPrefix(key, "MetricData.member.")
		if !ok || len(vs) == 0 {
			continue
		}
		idxS, field, ok := strings.Cut(rest, ".")
		idx, err := strconv.Atoi(idxS)
		if !ok || err != nil || idx < 1 || idx > 1000 {
			return nil, fmt.Errorf("bad parameter %q", key)
		}
		a, v := get(idx), vs[0]
		switch {
		case field == "MetricName":
			a.d.MetricName = v
		case field == "Unit":
			a.d.Unit = v
		case field == "Value":
			x, err := num(key, v)
			if err != nil {
				return nil, err
			}
			a.d.Value = model.Float(x)
		case field == "Timestamp":
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("%s: want ISO8601", key)
			}
			a.d.Timestamp = t.UnixMilli()
		case field == "StorageResolution":
			x, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("%s: bad integer", key)
			}
			a.d.StorageResolution = uint32(x)
		case strings.HasPrefix(field, "StatisticValues."):
			x, err := num(key, v)
			if err != nil {
				return nil, err
			}
			if a.sv == nil {
				a.sv = &model.StatisticSet{}
			}
			switch strings.TrimPrefix(field, "StatisticValues.") {
			case "SampleCount":
				a.sv.SampleCount = x
			case "Sum":
				a.sv.Sum = x
			case "Minimum":
				a.sv.Minimum = x
			case "Maximum":
				a.sv.Maximum = x
			}
		case strings.HasPrefix(field, "Dimensions.member."):
			dIdxS, dField, _ := strings.Cut(strings.TrimPrefix(field, "Dimensions.member."), ".")
			dIdx, err := strconv.Atoi(dIdxS)
			if err != nil || dIdx < 1 || dIdx > model.MaxDimensions {
				return nil, fmt.Errorf("bad parameter %q", key)
			}
			d, ok := a.dims[dIdx]
			if !ok {
				d = &model.Dimension{}
				a.dims[dIdx] = d
			}
			if dField == "Name" {
				d.Name = v
			} else if dField == "Value" {
				d.Value = v
			}
		case strings.HasPrefix(field, "Values.member."), strings.HasPrefix(field, "Counts.member."):
			isVal := strings.HasPrefix(field, "Values.")
			i, err := strconv.Atoi(field[strings.LastIndexByte(field, '.')+1:])
			if err != nil {
				return nil, fmt.Errorf("bad parameter %q", key)
			}
			x, err := num(key, v)
			if err != nil {
				return nil, err
			}
			if isVal {
				if a.vals == nil {
					a.vals = map[int]float64{}
				}
				a.vals[i] = x
			} else {
				if a.cnts == nil {
					a.cnts = map[int]float64{}
				}
				a.cnts[i] = x
			}
		}
	}
	idxs := make([]int, 0, len(byIdx))
	for i := range byIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var out []model.MetricDatum
	for _, i := range idxs {
		a := byIdx[i]
		dIdx := make([]int, 0, len(a.dims))
		for j := range a.dims {
			dIdx = append(dIdx, j)
		}
		sort.Ints(dIdx)
		for _, j := range dIdx {
			a.d.Dimensions = append(a.d.Dimensions, *a.dims[j])
		}
		a.d.StatisticValues = a.sv
		if len(a.vals) > 0 {
			vals, cnts := make([]float64, 0, len(a.vals)), make([]float64, 0, len(a.vals))
			for k := 1; k <= len(a.vals); k++ {
				vals = append(vals, a.vals[k])
				c, ok := a.cnts[k]
				if !ok {
					c = 1
				}
				cnts = append(cnts, c)
			}
			out = append(out, expandValues(a.d, vals, cnts)...)
			continue
		}
		out = append(out, a.d)
	}
	return out, nil
}

// --- responses -----------------------------------------------------------------

func writeXML(w http.ResponseWriter, root, reqID string) {
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<%s xmlns="%s"><ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata></%s>`, root, cwXMLNS, xmlEscape(reqID), root)
}

func writeAWSJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	_ = json.NewEncoder(w).Encode(v)
}

func awsError(w http.ResponseWriter, r *http.Request, isJSON bool, status int, code, msg string) {
	if isJSON {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"__type": code, "message": msg})
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<ErrorResponse xmlns="%s"><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>%s</RequestId></ErrorResponse>`,
		cwXMLNS, xmlEscape(code), xmlEscape(msg), xmlEscape(httpx.RequestID(r.Context())))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
