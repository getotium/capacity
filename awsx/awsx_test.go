package awsx

import (
	"context"
	"testing"
)

// When both an access key and secret are supplied, LoadConfig uses a static credentials
// provider that resolves those exact values offline — no default chain, no network.
func TestLoadConfig_StaticCredentials(t *testing.T) {
	t.Parallel()
	const ak, sk = "AKIAEXAMPLE0000TEST", "secret-value-xyz"

	cfg, err := LoadConfig(context.Background(), "us-east-1", WithStaticCredentials(ak, sk))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", cfg.Region)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != ak || creds.SecretAccessKey != sk {
		t.Errorf("creds = %q / %q, want %q / %q", creds.AccessKeyID, creds.SecretAccessKey, ak, sk)
	}
	if creds.SessionToken != "" {
		t.Errorf("session token = %q, want empty (IAM-user keys have none)", creds.SessionToken)
	}
	if creds.Source != "StaticCredentials" {
		t.Errorf("source = %q, want StaticCredentials", creds.Source)
	}
}

// An empty (or partially-empty) credential pair is a no-op: LoadConfig falls back to the AWS
// default credential chain, which resolves lazily. Building the config must not error — that
// is the fallback contract that makes the migration zero-downtime. We don't Retrieve() here
// (the chain could reach SSO/IMDS); a clean build is the assertion.
func TestLoadConfig_FallbackToDefaultChain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, ak, sk string
	}{
		{"both empty", "", ""},
		{"access only", "AKIA", ""},
		{"secret only", "", "sec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadConfig(context.Background(), "us-west-2",
				WithStaticCredentials(tc.ak, tc.sk)); err != nil {
				t.Fatalf("LoadConfig(%s): unexpected error: %v", tc.name, err)
			}
		})
	}
}
