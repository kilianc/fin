package main

import (
	"cmp"
	"os"

	"github.com/kilianc/fin/internal/sheets"
)

// fin's Google OAuth client, a Desktop app in the fin Google Cloud project
// with only the drive.file scope. Google treats a desktop client's secret as
// public: it names the app, and the user's consent is what grants access.
// FIN_GOOGLE_CLIENT_ID and FIN_GOOGLE_CLIENT_SECRET replace it, for forks
// and testing.
const (
	googleClientID     = "977972239326-ai04sj0hk8u9ml9s3guhuldqioktk5ar.apps.googleusercontent.com"
	googleClientSecret = "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"
)

func googleClient() sheets.OAuthClient {
	return sheets.OAuthClient{
		ID:     cmp.Or(os.Getenv("FIN_GOOGLE_CLIENT_ID"), googleClientID),
		Secret: cmp.Or(os.Getenv("FIN_GOOGLE_CLIENT_SECRET"), googleClientSecret),
	}
}
