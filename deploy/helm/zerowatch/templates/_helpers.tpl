{{/* Names */}}
{{- define "zerowatch.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "zerowatch.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "zerowatch.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "zerowatch.labels" -}}
helm.sh/chart: {{ include "zerowatch.chart" . }}
app.kubernetes.io/name: {{ include "zerowatch.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: zerowatch
{{- end -}}

{{/* (dict "ctx" $ "component" "storage") */}}
{{- define "zerowatch.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zerowatch.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "zerowatch.componentLabels" -}}
{{ include "zerowatch.labels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "zerowatch.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "zerowatch.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "zerowatch.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/* The Secret env vars read from: existingSecret, or the one this chart creates. */}}
{{- define "zerowatch.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "zerowatch.fullname" . -}}
{{- end -}}
{{- end -}}

{{/* Component that serves UI/API and ingest: the single pod, or query/ingest. */}}
{{- define "zerowatch.apiComponent" -}}
{{- if eq .Values.mode "cluster" -}}query{{- else -}}single{{- end -}}
{{- end -}}
{{- define "zerowatch.ingestComponent" -}}
{{- if eq .Values.mode "cluster" -}}ingest{{- else -}}single{{- end -}}
{{- end -}}

{{- define "zerowatch.storageHeadless" -}}
{{- printf "%s-storage-headless" (include "zerowatch.fullname" .) -}}
{{- end -}}

{{/*
Gossip seeds. cluster: every storage pod by its stable DNS name (the
headless service publishes not-ready addresses, so the first pod can resolve
itself). single: the single pod's gossip service, used by the agents.
*/}}
{{- define "zerowatch.seeds" -}}
{{- $fn := include "zerowatch.fullname" . -}}
{{- $port := int .Values.ports.gossip -}}
{{- if eq .Values.mode "cluster" -}}
{{- $hl := include "zerowatch.storageHeadless" . -}}
{{- $seeds := list -}}
{{- range $i := until (int .Values.storageNodes.replicas) -}}
{{- $seeds = append $seeds (printf "%s-storage-%d.%s.%s.svc.%s:%d" $fn $i $hl $.Release.Namespace $.Values.clusterDomain $port) -}}
{{- end -}}
{{- join "," $seeds -}}
{{- else -}}
{{- printf "%s-gossip.%s.svc.%s:%d" $fn .Release.Namespace .Values.clusterDomain $port -}}
{{- end -}}
{{- end -}}

{{- define "zerowatch.forecasterURL" -}}
{{- if .Values.forecaster.externalURL -}}
{{- .Values.forecaster.externalURL -}}
{{- else if .Values.forecaster.enabled -}}
{{- printf "http://%s-forecaster.%s.svc.%s:%d" (include "zerowatch.fullname" .) .Release.Namespace .Values.clusterDomain (int .Values.forecaster.port) -}}
{{- end -}}
{{- end -}}

{{- define "zerowatch.secretEnv" -}}
{{- $name := .name -}}{{- $key := .key -}}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ $name }}
      key: {{ $key }}
      optional: true
{{- end -}}

{{/*
Env shared by every ZeroWatch container. Pass (dict "ctx" $ "nodeNameFrom" "metadata.name"|"spec.nodeName").
*/}}
{{- define "zerowatch.env" -}}
{{- $v := .ctx.Values -}}
{{- $secret := include "zerowatch.secretName" .ctx -}}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_IP
  valueFrom:
    fieldRef:
      fieldPath: status.podIP
- name: NODE_NAME
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
- name: ZW_NODE_NAME
  valueFrom:
    fieldRef:
      fieldPath: {{ .nodeNameFrom | default "metadata.name" }}
- name: ZW_LOG_LEVEL
  value: {{ $v.log.level | quote }}
- name: ZW_LOG_JSON
  value: {{ $v.log.json | quote }}
- name: ZW_GENERATE
  value: {{ $v.generator.enabled | quote }}
- name: ZW_GENERATE_SERVICES
  value: {{ $v.generator.services | quote }}
- name: ZW_DATA_DIR
  value: /data
- name: ZW_HTTP_ADDR
  value: {{ printf ":%d" (int (default $v.ports.http .httpPort)) | quote }}
- name: ZW_OTLP_ADDR
  value: {{ printf ":%d" (int $v.ports.otlp) | quote }}
- name: ZW_PROM_ADDR
  value: {{ printf ":%d" (int $v.ports.prom) | quote }}
- name: ZW_STATSD_ADDR
  value: {{ printf ":%d" (int $v.ports.statsd) | quote }}
- name: ZW_GOSSIP_PORT
  value: {{ (default $v.ports.gossip .gossipPort) | quote }}
- name: ZW_RAW_RETENTION
  value: {{ $v.retention.raw | quote }}
- name: ZW_ROLLUP_RETENTION
  value: {{ $v.retention.rollup | quote }}
- name: ZW_MAX_SERIES
  value: {{ $v.limits.maxSeries | quote }}
- name: ZW_MAX_LOG_EVENTS
  value: {{ $v.limits.maxLogEvents | quote }}
- name: ZW_RATE_LIMIT_RPS
  value: {{ $v.limits.rateLimitRPS | quote }}
- name: ZW_RATE_LIMIT_BURST
  value: {{ $v.limits.rateLimitBurst | quote }}
{{- with $v.corsOrigins }}
- name: ZW_CORS_ORIGINS
  value: {{ join "," . | quote }}
{{- end }}
- name: ZW_STORAGE
  value: {{ $v.storage.backend | quote }}
{{- if eq $v.storage.backend "clickhouse" }}
- name: ZW_CLICKHOUSE_URL
  value: {{ $v.storage.clickhouse.url | quote }}
- name: ZW_CLICKHOUSE_DB
  value: {{ $v.storage.clickhouse.database | quote }}
{{- end }}
{{- if $v.kafka.enabled }}
- name: ZW_COMMITLOG
  value: kafka
- name: ZW_KAFKA_BROKERS
  value: {{ join "," $v.kafka.brokers | quote }}
- name: ZW_KAFKA_TOPIC
  value: {{ $v.kafka.topic | quote }}
{{- else }}
- name: ZW_COMMITLOG
  value: memory
{{- end }}
{{- with (include "zerowatch.forecasterURL" .ctx) }}
- name: ZW_FORECASTER_URL
  value: {{ . | quote }}
{{- end }}
- name: ZW_REPORT_EVERY
  value: {{ $v.reports.every | quote }}
- name: ZW_REPORT_RANGE
  value: {{ $v.reports.range | quote }}
{{- if $v.metadata.dsn }}
- name: ZW_METADATA_DSN
  value: {{ $v.metadata.dsn | quote }}
{{- end }}
{{- if $v.reports.targets }}
- name: ZW_REPORT_TARGETS
  value: {{ join "," $v.reports.targets | quote }}
{{- end }}
{{- if $secret }}
{{- $k := $v.secrets.keys }}
{{ include "zerowatch.secretEnv" (dict "env" "ZW_API_KEYS" "name" $secret "key" $k.apiKeys) }}
{{ include "zerowatch.secretEnv" (dict "env" "ZW_AWS_CREDENTIALS" "name" $secret "key" $k.awsCredentials) }}
{{ include "zerowatch.secretEnv" (dict "env" "ZW_CLUSTER_SECRET" "name" $secret "key" $k.clusterSecret) }}
{{- if not $v.metadata.dsn }}
{{ include "zerowatch.secretEnv" (dict "env" "ZW_METADATA_DSN" "name" $secret "key" $k.metadataDSN) }}
{{- end }}
{{ include "zerowatch.secretEnv" (dict "env" "ZW_CLICKHOUSE_USER" "name" $secret "key" $k.clickhouseUser) }}
{{ include "zerowatch.secretEnv" (dict "env" "ZW_CLICKHOUSE_PASSWORD" "name" $secret "key" $k.clickhousePassword) }}
{{- if not $v.reports.targets }}
{{ include "zerowatch.secretEnv" (dict "env" "ZW_REPORT_TARGETS" "name" $secret "key" $k.reportTargets) }}
{{- end }}
{{ include "zerowatch.secretEnv" (dict "env" "AWS_ACCESS_KEY_ID" "name" $secret "key" $k.awsAccessKeyId) }}
{{ include "zerowatch.secretEnv" (dict "env" "AWS_SECRET_ACCESS_KEY" "name" $secret "key" $k.awsSecretAccessKey) }}
{{ include "zerowatch.secretEnv" (dict "env" "AWS_SESSION_TOKEN" "name" $secret "key" $k.awsSessionToken) }}
{{- end }}
{{- with $v.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* Common container pieces */}}
{{- define "zerowatch.probes" -}}
{{- $p := .ctx.Values.probes -}}
livenessProbe:
  httpGet:
    path: {{ $p.liveness.path }}
    port: {{ .port }}
  initialDelaySeconds: {{ $p.liveness.initialDelaySeconds }}
  periodSeconds: {{ $p.liveness.periodSeconds }}
  timeoutSeconds: {{ $p.liveness.timeoutSeconds }}
  failureThreshold: {{ $p.liveness.failureThreshold }}
readinessProbe:
  httpGet:
    path: {{ $p.readiness.path }}
    port: {{ .port }}
  initialDelaySeconds: {{ $p.readiness.initialDelaySeconds }}
  periodSeconds: {{ $p.readiness.periodSeconds }}
  timeoutSeconds: {{ $p.readiness.timeoutSeconds }}
  failureThreshold: {{ $p.readiness.failureThreshold }}
{{- end -}}

{{- define "zerowatch.podMeta" -}}
annotations:
  checksum/secret: {{ include (print .ctx.Template.BasePath "/secret.yaml") .ctx | sha256sum }}
  {{- with .ctx.Values.podAnnotations }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
labels:
  {{- include "zerowatch.selectorLabels" . | nindent 2 }}
  {{- with .ctx.Values.podLabels }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end -}}

{{/* Pod-level fields shared by the server components. */}}
{{- define "zerowatch.podCommon" -}}
{{- $v := .Values -}}
serviceAccountName: {{ include "zerowatch.serviceAccountName" . }}
automountServiceAccountToken: {{ $v.serviceAccount.automountToken }}
{{- with $v.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
securityContext:
  {{- toYaml $v.podSecurityContext | nindent 2 }}
{{- with $v.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
{{- with $v.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $v.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $v.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/* Container ports for a server component. */}}
{{- define "zerowatch.containerPorts" -}}
{{- $v := .ctx.Values -}}
- name: http
  containerPort: {{ $v.ports.http }}
  protocol: TCP
{{- if .ingest }}
- name: otlp
  containerPort: {{ $v.ports.otlp }}
  protocol: TCP
- name: prom
  containerPort: {{ $v.ports.prom }}
  protocol: TCP
- name: statsd
  containerPort: {{ $v.ports.statsd }}
  protocol: UDP
{{- end }}
{{- if .gossip }}
- name: gossip-tcp
  containerPort: {{ $v.ports.gossip }}
  protocol: TCP
- name: gossip-udp
  containerPort: {{ $v.ports.gossip }}
  protocol: UDP
{{- end }}
{{- end -}}


{{/*
Init container that waits until a gossip seed accepts TCP. ZeroWatch joins its
seeds once at startup (no retry), and pods that start together otherwise race
the headless-service DNS records. storage-0 seeds itself and never waits.
*/}}
{{- define "zerowatch.seedWait" -}}
{{- $v := .Values -}}
{{- if $v.seedWait.enabled }}
initContainers:
  - name: wait-for-seeds
    image: {{ $v.seedWait.image }}
    imagePullPolicy: IfNotPresent
    env:
      - name: POD_NAME
        valueFrom:
          fieldRef:
            fieldPath: metadata.name
      - name: SEEDS
        value: {{ include "zerowatch.seeds" . | quote }}
      - name: TIMEOUT
        value: {{ $v.seedWait.timeoutSeconds | quote }}
      - name: SETTLE
        value: {{ $v.seedWait.settleSeconds | quote }}
    command:
      - sh
      - -c
      - |
        case "$POD_NAME" in *-storage-0) exit 0;; esac
        # Exit once a seed accepts TCP and no lookup has failed for SETTLE
        # seconds, so NXDOMAIN answers CoreDNS cached before the record
        # existed have expired (ZeroWatch resolves its seeds only once).
        start=$(date +%s); lastfail=0
        while :; do
          now=$(date +%s); ok=""
          for s in $(echo "$SEEDS" | tr ',' ' '); do
            h="${s%:*}"; p="${s##*:}"
            case "$h" in "$POD_NAME".*) continue;; esac
            if nc -z -w 2 "$h" "$p" 2>/dev/null; then ok="$s"; break; fi
          done
          if [ -z "$ok" ]; then lastfail=$now; fi
          if [ -n "$ok" ] && { [ "$lastfail" -eq 0 ] || [ $((now - lastfail)) -ge "$SETTLE" ]; }; then
            echo "seed $ok reachable after $((now - start))s"; exit 0
          fi
          if [ $((now - start)) -ge "$TIMEOUT" ]; then echo "no seed settled after ${TIMEOUT}s; starting anyway"; exit 0; fi
          sleep 2
        done
    securityContext:
      runAsNonRoot: true
      runAsUser: 65532
      runAsGroup: 65532
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
    resources:
      requests: { cpu: 5m, memory: 8Mi }
      limits: { memory: 32Mi }
{{- end }}
{{- end -}}
