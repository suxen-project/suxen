package content

import (
	"io"
	"net/http"

	"github.com/suxen-project/suxen/internal/ocimodel"
)

func (rt *Runtime) StageOCIManifest(
	w http.ResponseWriter,
	source io.Reader,
) (StagedUpload, error) {
	staged, err := rt.StageUpload(w, io.LimitReader(source, ocimodel.MaxManifestBytes+1))
	if err != nil {
		return StagedUpload{}, err
	}
	if staged.Size > ocimodel.MaxManifestBytes {
		staged.remove()
		return StagedUpload{}, ocimodel.ErrManifestTooLarge
	}
	return staged, nil
}
