package main

import (
	"strings"
	"testing"
)

func TestUnexpectedArgumentsDoNotRunCommands(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
	}{
		{name: "help", arguments: []string{"help", "extra"}},
		{name: "version", arguments: []string{"version", "extra"}},
		{name: "stats", arguments: []string{"stats", "extra"}},
		{name: "repositories", arguments: []string{"repo", "list", "extra"}},
		{name: "roles", arguments: []string{"role", "list", "extra"}},
		{name: "users", arguments: []string{"user", "list", "extra"}},
		{name: "cleanup policies", arguments: []string{"cleanup-policy", "list", "extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A nil client makes an accidental HTTP request fail the test immediately.
			err := dispatch(nil, test.arguments)
			if err == nil || !strings.HasPrefix(err.Error(), "usage:") {
				t.Fatalf("dispatch(%q) = %v, want usage error", test.arguments, err)
			}
		})
	}
}

func TestUserRolesRejectsConflictingOrEmptyMutationFlags(t *testing.T) {
	for _, arguments := range [][]string{
		{"--set", "writer", "--clear", "ci"},
		{"--set", "", "ci"},
		{"--set", ",", "ci"},
		{"--set", " , , ", "ci"},
	} {
		err := userRoles(nil, arguments)
		if err == nil {
			t.Fatalf("user roles %q unexpectedly succeeded", arguments)
		}
	}
}
