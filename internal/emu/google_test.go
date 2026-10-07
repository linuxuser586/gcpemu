package emu

import "testing"

func TestIsGoogleHost(t *testing.T) {
	for host, want := range map[string]bool{
		"oauth2.googleapis.com": true, "storage.googleapis.com:443": true, "accounts.google.com": true,
		"securetoken.google.com.": true, "GOOGLEAPIS.COM": true, "lh3.googleusercontent.com": true,
		"token.actions.githubusercontent.com": false, "notgoogle.com": false, "googleapis.com.evil.test": false,
		"us-docker.pkg.dev": false, "127.0.0.1:4510": false, "": false,
	} {
		if got := IsGoogleHost(host); got != want {
			t.Errorf("IsGoogleHost(%q) = %v, want %v", host, got, want)
		}
	}
}
