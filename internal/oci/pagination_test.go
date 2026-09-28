package oci

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestPaginateNames(t *testing.T) {
	names := []string{"acme/alpha", "acme/beta", "acme/gamma"}
	page, next, more := paginateOCINames(names, PageRequest{N: 2, NSet: true})
	if strings.Join(page, ",") != "acme/alpha,acme/beta" || next != "acme/beta" || !more {
		t.Fatalf("first page = %v next=%q more=%v", page, next, more)
	}
	page, next, more = paginateOCINames(names, PageRequest{N: 2, NSet: true, Last: "acme/beta"})
	if strings.Join(page, ",") != "acme/gamma" || next != "" || more {
		t.Fatalf("last page = %v next=%q more=%v", page, next, more)
	}
	page, _, more = paginateOCINames(names, PageRequest{})
	if len(page) != 3 || more {
		t.Fatalf("unset n still returns remaining names = %v more=%v", page, more)
	}
}

func TestParsePageRequestDefaultsAndCaps(t *testing.T) {
	page, err := ParsePageRequest(url.Values{})
	if err != nil || page.N != defaultOCIPageSize || !page.NSet {
		t.Fatalf("default page = %+v err=%v", page, err)
	}
	page, err = ParsePageRequest(url.Values{"n": []string{"25"}})
	if err != nil || page.N != 25 {
		t.Fatalf("explicit n = %+v err=%v", page, err)
	}
	if _, err := ParsePageRequest(url.Values{"n": []string{"1001"}}); err == nil {
		t.Fatal("n above maxOCIPageSize unexpectedly succeeded")
	}
	if _, err := ParsePageRequest(url.Values{"n": []string{"-1"}}); err == nil {
		t.Fatal("negative n unexpectedly succeeded")
	}

	names := make([]string, defaultOCIPageSize+1)
	for i := range names {
		names[i] = fmt.Sprintf("img-%03d", i)
	}
	defaultPage, err := ParsePageRequest(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	first, next, more := paginateOCINames(names, defaultPage)
	if !more || len(first) != defaultOCIPageSize || next != names[defaultOCIPageSize-1] {
		t.Fatalf("default catalog page len=%d next=%q more=%v", len(first), next, more)
	}
}

func TestOCIContinuationStaysOnConfiguredUpstreamEndpoint(t *testing.T) {
	upstream := "https://registry.example/repository/source"
	current := "v2/acme/app/tags/list?n=1"
	for _, test := range []struct {
		link string
		want string
	}{
		{
			link: "https://registry.example/repository/source/v2/acme/app/tags/list?n=1&last=first",
			want: "v2/acme/app/tags/list?n=1&last=first",
		},
		{link: "?n=1&last=first", want: "v2/acme/app/tags/list?n=1&last=first"},
	} {
		got, err := ociAssetPathFromLink(upstream, current, test.link)
		if err != nil || got != test.want {
			t.Fatalf("continuation %q = %q, %v; want %q", test.link, got, err, test.want)
		}
	}
	for _, link := range []string{
		"https://elsewhere.example/repository/source/v2/acme/app/tags/list?last=first",
		"https://registry.example/repository/other/v2/acme/app/tags/list?last=first",
		"https://registry.example/repository/source/v2/other/tags/list?last=first",
		"https://user:secret@registry.example/repository/source/v2/acme/app/tags/list?last=first",
	} {
		if _, err := ociAssetPathFromLink(upstream, current, link); err == nil {
			t.Fatalf("continuation %q escaped configured upstream endpoint", link)
		}
	}
}
