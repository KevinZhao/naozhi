package agentcore

import (
	"strings"
	"testing"
)

func TestVerifyARNAccount(t *testing.T) {
	const self = "123456789012"
	const validARN = "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/my-runtime"

	tests := []struct {
		name      string
		arn       string
		accountID string
		wantErr   bool
		// errSubstr, when set, must appear in the returned error.
		errSubstr string
	}{
		{
			name:      "match",
			arn:       validARN,
			accountID: self,
			wantErr:   false,
		},
		{
			name:      "account mismatch",
			arn:       "arn:aws:bedrock-agentcore:us-east-1:999988887777:runtime/attacker",
			accountID: self,
			wantErr:   true,
			errSubstr: "does not match caller account",
		},
		{
			name:      "malformed arn missing sections",
			arn:       "arn:aws:bedrock-agentcore",
			accountID: self,
			wantErr:   true,
			errSubstr: "invalid RuntimeARN",
		},
		{
			name:      "not an arn at all",
			arn:       "not-an-arn",
			accountID: self,
			wantErr:   true,
			errSubstr: "invalid RuntimeARN",
		},
		{
			name:      "empty arn",
			arn:       "",
			accountID: self,
			wantErr:   true,
			errSubstr: "invalid RuntimeARN",
		},
		{
			name:      "empty caller account id",
			arn:       validARN,
			accountID: "",
			wantErr:   true,
			errSubstr: "empty caller account id",
		},
		{
			name:      "aws-cn partition matching account",
			arn:       "arn:aws-cn:bedrock-agentcore:cn-north-1:123456789012:runtime/cn",
			accountID: self,
			wantErr:   false,
		},
		{
			name:      "wrong service lambda same account",
			arn:       "arn:aws:lambda:us-east-1:123456789012:function:evil",
			accountID: self,
			wantErr:   true,
			errSubstr: "is not bedrock-agentcore",
		},
		{
			name:      "wrong service s3 same account",
			arn:       "arn:aws:s3:::my-bucket/key",
			accountID: self,
			wantErr:   true,
			errSubstr: "is not bedrock-agentcore",
		},
		{
			name:      "empty region",
			arn:       "arn:aws:bedrock-agentcore::123456789012:runtime/no-region",
			accountID: self,
			wantErr:   true,
			errSubstr: "empty region",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyARNAccount(tt.arn, tt.accountID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("verifyARNAccount(%q, %q) = nil, want error", tt.arn, tt.accountID)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyARNAccount(%q, %q) = %v, want nil", tt.arn, tt.accountID, err)
			}
		})
	}
}
