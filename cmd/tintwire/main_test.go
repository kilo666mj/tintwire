package main

import "testing"

func TestAuthenticatedDeploymentRequiresPublicURL(t *testing.T) {
	if err := validateAuthConfiguration(true, ""); err == nil {
		t.Fatal("authenticated deployment without a public URL was accepted")
	}
	if err := validateAuthConfiguration(true, "   "); err == nil {
		t.Fatal("authenticated deployment with a blank public URL was accepted")
	}
	if err := validateAuthConfiguration(true, "https://tintwire.example"); err != nil {
		t.Fatalf("authenticated deployment with a public URL was rejected: %v", err)
	}
	if err := validateAuthConfiguration(false, ""); err != nil {
		t.Fatalf("local unauthenticated deployment was rejected: %v", err)
	}
}
