package transcribe

import (
	"context"
	"errors"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// CheckCredentials resolves the AWS credential chain New's client uses and
// returns the provider name that supplied credentials. New never retrieves
// credentials, so without this a missing chain only surfaces when the first
// voice message fails. Key material is never returned.
func CheckCredentials(ctx context.Context, region string) (string, error) {
	if region == "" {
		region = "us-east-1"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return "", fmt.Errorf("load aws config: %w", err)
	}
	if awsCfg.Credentials == nil {
		return "", errors.New("no aws credential provider configured")
	}
	creds, err := awsCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("retrieve aws credentials: %w", err)
	}
	return creds.Source, nil
}

// FFmpegPath resolves ffmpeg exactly as audio conversion does; formats other
// than ogg, flac and pcm cannot be transcribed without it.
func FFmpegPath() (string, error) {
	return lookupFFmpeg()
}
