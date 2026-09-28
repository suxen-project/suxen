package content

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// selectLocatedGroupArtifactMember discovers the source/filename from stored
// indexes, then checks that filename in every member in group order. A later
// member's URL never wins over an earlier member's same filename.
func (rt *Runtime) selectLocatedGroupArtifactMember(
	w http.ResponseWriter, r *http.Request, group domain.Repository,
	request spiformat.GroupArtifactRequest, locator spiformat.GroupArtifactLocator, storedOnly bool,
) (owner string, matches, handled bool, err error) {
	var candidate *spiformat.GroupArtifactCandidate
	selected := ""
	selectedMatches := false
	for _, name := range group.Members {
		member, err := rt.repositoryResolver().Repository(r.Context(), name)
		if err != nil {
			return "", false, false, fmt.Errorf("load group member %q: %w", name, err)
		}
		if member.Format != group.Format {
			continue
		}
		if member.Type == "group" {
			return "", false, false, fmt.Errorf("group %q contains nested group %q", group.Name, name)
		}
		found, applicable, err := locator.GroupArtifactCandidates(r.Context(), member.FormatView(), request,
			storedAssetsView{runtime: rt, repository: member})
		if err != nil {
			return "", false, false, fmt.Errorf("locate group member %q artifact: %w", name, err)
		}
		handled = handled || applicable
		for _, next := range found {
			if candidate != nil && *candidate != next {
				return "", false, true, fmt.Errorf("ambiguous group artifact source for %q", request.Path)
			}
			candidate = &next
		}
	}
	if !handled || candidate == nil {
		return "", false, handled, nil
	}
	for _, name := range group.Members {
		member, err := rt.repositoryResolver().Repository(r.Context(), name)
		if err != nil {
			return "", false, true, fmt.Errorf("load group member %q: %w", name, err)
		}
		if member.Format != group.Format {
			continue
		}
		var body []byte
		var found bool
		if storedOnly {
			body, found, err = rt.storedGroupSourceContent(r, member, candidate.SourcePath)
		} else {
			body, _, found, err = rt.memberSourceContent(w, r, member, candidate.SourcePath)
		}
		if err != nil {
			return "", false, true, fmt.Errorf("collect from group member %q: %w", name, err)
		}
		if !found {
			continue
		}
		owns, matches, err := locator.GroupSourceArtifact(member.FormatView(), candidate.SourcePath,
			candidate.ArtifactKey, request, body)
		if err != nil {
			var violation *spiformat.PolicyViolation
			if errors.As(err, &violation) {
				return "", false, true, nil
			}
			return "", false, true, fmt.Errorf("inspect group member %q: %w", name, err)
		}
		if owns && selected == "" {
			selected = name
			selectedMatches = matches
		}
	}
	return selected, selectedMatches, true, nil
}
