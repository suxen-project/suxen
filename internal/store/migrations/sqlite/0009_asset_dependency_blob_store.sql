-- Record the home blob store of each depended-on blob on its dependency edge.
-- NULL means the edge's home store was not recorded; it is never inferred from the
-- manifest's own store, and an edge may remain NULL even after its blob is
-- published (a writer that did not resolve it, or a publication-order race).
-- Reference tracking pins a NULL edge's digest in every store only while no
-- oci-blob asset row exists for that digest anywhere (a proxy manifest cached
-- before its layers are pulled); as soon as any blob row exists, that row is the
-- authoritative record of the blob's location and the all-store pin drops,
-- whether or not the edge was ever resolved.
ALTER TABLE asset_dependencies ADD COLUMN blob_store TEXT;

-- Backfill existing edges: resolve any whose blob is already published (to a store
-- that currently holds it), leaving the rest UNRESOLVED. Pre-migration edges whose
-- blob is absent were invisible to reference tracking before; marking them
-- UNRESOLVED now pins them, which is strictly safer.
UPDATE asset_dependencies
SET blob_store = (
    SELECT MIN(blob_store)
    FROM assets
    WHERE kind = 'oci-blob' AND digest = asset_dependencies.digest
)
WHERE EXISTS (
    SELECT 1
    FROM assets
    WHERE kind = 'oci-blob' AND digest = asset_dependencies.digest
);
