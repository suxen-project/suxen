package provision

import (
	"reflect"
	"testing"
)

func TestBuiltInResourcesStartPrivate(t *testing.T) {
	resources := BuiltInResources()
	byKey := make(map[string]Resource, len(resources))
	for _, resource := range resources {
		byKey[resource.Kind+"/"+resource.Name] = resource
	}

	for _, repository := range []string{"raw", "oci"} {
		resource, found := byKey["repository/"+repository]
		if !found {
			t.Fatalf("built-in repository %q is missing", repository)
		}
		if resource.Spec["format"] != repository || resource.Spec["type"] != "hosted" {
			t.Fatalf("built-in repository %q = %+v", repository, resource.Spec)
		}
	}

	anonymous, found := byKey["role/anonymous"]
	if !found {
		t.Fatal("built-in anonymous role is missing")
	}
	if privileges, ok := anonymous.Spec["privileges"].([]string); !ok || len(privileges) != 0 {
		t.Fatalf("anonymous privileges = %#v, want an empty list", anonymous.Spec["privileges"])
	}

	administrator, found := byKey["role/administrator"]
	if !found {
		t.Fatal("built-in administrator role is missing")
	}
	if !reflect.DeepEqual(administrator.Spec["privileges"], []string{"*"}) {
		t.Fatalf("administrator privileges = %#v, want [*]", administrator.Spec["privileges"])
	}
}
