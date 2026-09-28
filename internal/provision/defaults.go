package provision

// BuiltInResources returns the baseline repositories and roles created when
// absent from a Suxen server. Existing resources retain explicit operator state,
// and built-in identities remain protected from explicit prune operations.
func BuiltInResources() []Resource {
	return []Resource{
		{
			Kind: "repository",
			Name: "raw",
			Spec: map[string]any{
				"format": "raw",
				"type":   "hosted",
			},
		},
		{
			Kind: "repository",
			Name: "oci",
			Spec: map[string]any{
				"format": "oci",
				"type":   "hosted",
			},
		},
		{
			Kind: "role",
			Name: "anonymous",
			Spec: map[string]any{
				"description": "Privileges granted to unauthenticated requests",
				"privileges":  []string{},
			},
		},
		{
			Kind: "role",
			Name: "administrator",
			Spec: map[string]any{
				"description": "Unrestricted repository and administrative access",
				"privileges":  []string{"*"},
			},
		},
	}
}
