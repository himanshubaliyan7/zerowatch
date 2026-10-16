package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Role != RoleAll || !c.Generate || !c.Supervisor || c.HTTPAddr != ":8080" || c.AuthEnabled() {
		t.Fatalf("unexpected defaults %+v", c)
	}
	if c.MetadataDSN != "sqlite:./data/metadata.db" {
		t.Fatal(c.MetadataDSN)
	}
}

func TestLoadFlags(t *testing.T) {
	c, err := Load([]string{"--role=query", "--api-keys=abcdefghijklmnop:7,qrstuvwxyz012345", "--generate=true"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Supervisor || !c.Generate || len(c.APIKeys) != 2 || c.APIKeys[0].TenantID != 7 || c.APIKeys[1].TenantID != 1 {
		t.Fatalf("%+v", c)
	}
	if !c.Runs(RoleQuery) || c.Runs(RoleIngest) {
		t.Fatal("runs")
	}
}

func TestLoadErrors(t *testing.T) {
	for _, args := range [][]string{{"--role=bogus"}, {"--api-keys=short"}, {"--storage=s3"}, {"--metadata-dsn=mysql://x"}} {
		if _, err := Load(args); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
}

func TestAWSCredentials(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	c, err := Load([]string{"--aws-credentials=AKIDEXAMPLE:" + secret + ":3, AKID2:0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AWSCredentials) != 2 || c.AWSCredentials[0].AccessKeyID != "AKIDEXAMPLE" || c.AWSCredentials[0].Secret != secret ||
		c.AWSCredentials[0].TenantID != 3 || c.AWSCredentials[1].TenantID != 1 || !c.AuthEnabled() {
		t.Fatalf("%v", c.AWSCredentials)
	}
	if s := fmt.Sprintf("%v %+v %s", c.AWSCredentials, c, c.AWSCredentials[0]); strings.Contains(s, secret) {
		t.Fatalf("secret leaked when formatting: %s", s)
	}
	t.Setenv("ZW_AWS_CREDENTIALS", "AKIDENV:"+secret+":9")
	if c, err = Load(nil); err != nil || len(c.AWSCredentials) != 1 || c.AWSCredentials[0].TenantID != 9 {
		t.Fatalf("env: %v %v", c, err)
	}
	for _, bad := range []string{"AKID", "AKID:short:1", "AKID:" + secret + ":0", "AKID:" + secret + ":x", ":" + secret, "A:" + secret + ",A:" + secret, "A:" + secret + ":1:2"} {
		_, err := ParseAWSCredentials(bad)
		if err == nil {
			t.Fatalf("expected error for %q", bad)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "short") {
			t.Fatalf("error leaks secret: %v", err)
		}
	}
}
