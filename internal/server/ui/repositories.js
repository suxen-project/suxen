"use strict";

import {api, APIError, apiPath, queryString} from "./api.js";
import {
  badge,
  button,
  code,
  confirmAction,
  cursorPager,
  definitionList,
  element,
  formatBytes,
  formatDate,
  heading,
  openForm,
  numberedPager,
  prettyJSON,
  renderTable,
  routeLink,
  showNotice,
} from "./components.js";
import {cursorPageHistory} from "./pagination.js";
import {hasPrivilege, routePath} from "./router.js";
import {repositoryRouteKind} from "./route_shapes.js";
import {defaults, schemas} from "./schemas.js";
import {navigateOrRefresh, refreshCurrentRoute} from "./navigation.js";
import {predicateSummary} from "./resource_views.js";
import {cloneJSON} from "./json_codec.js";

export async function renderRepositoryRoute(view, route, identity) {
  const [section, repositoryName, child, identifier] = route.segments;
  const routeKind = repositoryRouteKind(route.segments);
  if (routeKind === "browse") {
    await renderBrowse(view, repositoryName, route.query);
    return true;
  }
  if (routeKind === "search") {
    await renderSearch(view, route.query);
    return true;
  }
  if (!routeKind) {
    return false;
  }
  if (routeKind === "repository-list") {
    await renderRepositoryList(view, identity);
    return true;
  }
  if (routeKind === "asset-detail") {
    await renderAssetDetail(view, repositoryName, identifier, identity);
    return true;
  }
  if (routeKind === "asset-list") {
    await renderAssets(view, repositoryName, identity);
    return true;
  }
  if (routeKind === "cleanup") {
    await renderCleanupPreview(view, repositoryName, route.query, identity);
    return true;
  }
  await renderRepositoryDetail(view, repositoryName, identity);
  return true;
}

async function renderRepositoryList(view, identity) {
  const canWrite = hasPrivilege(identity, "admin:repositories:write");
  view.replaceChildren(heading(
    "Repositories",
    "Hosted, proxy, and group endpoints for the installed repository formats.",
    [canWrite ? button("Create repository", () => editRepository(null)) : null],
  ));
  const panel = element("div", "panel");
  const table = element("div");
  const controls = element("div");
  panel.append(table, controls);
  view.append(panel);
  const pageSize = 50;
  const load = async (requestedPage) => {
    const page = await api.page("/api/v1/repositories", {limit: pageSize, page: requestedPage});
    const columns = [
      {label: "Name", render: (repository) => code(repository.name)},
      {label: "Format", key: "format"},
      {label: "Type", render: (repository) => badge(repository.type)},
      {label: "Blob store", key: "blobStore"},
      {label: "Target", render: repositoryTarget},
      {label: "Ownership", render: (repository) => repository.managed ? badge("managed") : "manual"},
      {label: "Created", render: (repository) => formatDate(repository.createdAt)},
    ];
    if (canWrite) {
      columns.push({
        label: "",
        // Managed repositories are reconciled from the provisioning document, so they
        // stay non-deletable here to match the detail-page and edit-dialog gating.
        render: (repository) => repository.managed
          ? ""
          : button("Delete", () => removeRepositoryFromList(repository), "danger small"),
      });
    }
    renderTable(table, columns, page.items, {
      empty: "No repositories are configured.",
      href: (repository) => routePath("repositories", repository.name),
    });
    controls.replaceChildren(numberedPager(requestedPage, page.total || 0, pageSize, load));
  };
  await load(1);
}

async function renderRepositoryDetail(view, repositoryName, identity) {
  const repository = await api.json(apiPath("api", "v1", "repositories", repositoryName));
  const actions = [
    routeLink("Assets", routePath("repositories", repositoryName, "assets"), "button"),
    routeLink("Browse", routePath("browse", repositoryName), "button secondary"),
  ];
  if (hasPrivilege(identity, "admin:repositories:write") && !repository.managed) {
    actions.push(button("Edit", () => editRepository(repository)));
    actions.push(button("Delete", () => deleteRepository(repository), "danger"));
  }
  if (hasPrivilege(identity, `repository:${repositoryName}:manage`)) {
    actions.push(button("Classification", () => editRepositorySetting(repository, "classification"), "secondary"));
    if (repository.type !== "group") {
      actions.push(button("Download gate", () => editRepositorySetting(repository, "download-gate"), "secondary"));
      actions.push(button("Trust policy", () => editRepositorySetting(repository, "trust-policy"), "secondary"));
    }
  }
  if (hasPrivilege(identity, `repository:${repositoryName}:delete`)) {
    actions.push(button("Preview cleanup", () => chooseCleanupPolicy(repositoryName), "secondary"));
  }
  view.replaceChildren(heading(
    `Repository · ${repository.name}`,
    `${repository.format.toUpperCase()} ${repository.type}`,
    [repository.managed ? badge("managed") : null, ...actions],
  ));
  const panel = element("div", "panel detail-panel");
  panel.append(definitionList([
    ["Name", code(repository.name)],
    ["Format", repository.format],
    ["Type", badge(repository.type)],
    [
      "Blob store",
      routeLink(
        repository.blobStore || "default",
        routePath("blob-stores", repository.blobStore || "default"),
      ),
    ],
    ["Upstream / members", repositoryTarget(repository)],
    ["OCI endpoints", repositoryEndpoints(repository)],
    ["Writable", badge(repository.writable ? "enabled" : "disabled")],
    ["Replace existing assets", repository.type === "hosted" ? (repository.allowOverwrite ? "Allowed" : "Denied") : "Not applicable"],
    ["Created", formatDate(repository.createdAt)],
  ]));
  view.append(panel);
  if (hasPrivilege(identity, `repository:${repositoryName}:read`)) {
    try {
      view.append(await contentPolicyPanel(repository));
    } catch (error) {
      showNotice(`Could not load content policies: ${error.message}`, true);
    }
  }
  if (repository.format === "raw" && repository.type === "hosted" &&
      hasPrivilege(identity, `repository:${repositoryName}:write`)) {
    view.append(uploadPanel(repositoryName));
  }
}

async function fetchRepositorySetting(repositoryName, setting) {
  try {
    return await api.json(apiPath("api", "v1", "repositories", repositoryName, setting));
  } catch (error) {
    if (error instanceof APIError && error.status === 404) {
      return null;
    }
    throw error;
  }
}

// Read-only summary of the three per-repository content policies so their state
// is visible on the detail page without opening each editor modal.
async function contentPolicyPanel(repository) {
  const repositoryName = repository.name;
  const [classification, gate, trust] = await Promise.all([
    fetchRepositorySetting(repositoryName, "classification"),
    repository.type === "group" ? null : fetchRepositorySetting(repositoryName, "download-gate"),
    repository.type === "group" ? null : fetchRepositorySetting(repositoryName, "trust-policy"),
  ]);
  const panel = element("section", "panel");
  panel.append(element("h2", "", "Content policies"));
  panel.append(definitionList([
    ["Classification", classification
      ? `${(classification.rules || []).length} rule(s) · ` +
        `${classification.inheritGlobal === false ? "instance default ignored" : "inherits instance default"}`
      : "Not configured"],
    ["Download gate", repository.type === "group"
      ? "Enforced by member repositories (including their instance defaults)"
      : gate
      ? `${gate.enabled ? "Enabled" : "Disabled"} · ${predicateSummary(gate.criteria)} · ` +
        `${gate.inheritGlobal ? "inherits instance default" : "instance default ignored"}`
      : "Not configured (inherits instance default)"],
    ["Trust policy", repository.type === "group"
      ? "Enforced by member repositories (including their instance defaults)"
      : trust
      ? `Mode ${trust.mode} · ${(trust.publicKeys || []).length} key(s), ` +
        `${(trust.allowedIdentities || []).length} identity rule(s)`
      : "Not configured (inherits instance default)"],
  ]));
  return panel;
}

async function deleteRepository(repository) {
  if (!await confirmAction(
    `Delete repository ${repository.name} and all of its asset metadata?`,
  )) {
    return;
  }
  await api.json(apiPath("api", "v1", "repositories", repository.name), {method: "DELETE"});
  showNotice(`Repository ${repository.name} was deleted.`);
  window.location.assign(routePath("repositories"));
}

async function removeRepositoryFromList(repository) {
  if (!await confirmAction(
    `Delete repository ${repository.name} and all of its asset metadata?`,
  )) {
    return;
  }
  await api.json(apiPath("api", "v1", "repositories", repository.name), {method: "DELETE"});
  showNotice(`Repository ${repository.name} was deleted.`);
  // Already on the repositories route, so refresh in place rather than navigating.
  window.dispatchEvent(new CustomEvent("suxen:resource-changed"));
}

function repositoryTarget(repository) {
  return repository.upstream || repository.members?.join(", ") || "Local storage";
}

function repositoryEndpoints(repository) {
  const hosts = repository.endpoints?.hosts || [];
  const ports = repository.endpoints?.ports || [];
  if (hosts.length === 0 && ports.length === 0) {
    return repository.format === "oci" ? "/repository/" + repository.name + "/v2/" : "—";
  }
  const parts = [];
  if (hosts.length > 0) {
    parts.push(hosts.join(", "));
  }
  if (ports.length > 0) {
    parts.push("ports " + ports.join(", "));
  }
  return parts.join("; ");
}

let formatOptions = null;

async function loadFormatOptions() {
  if (formatOptions) {
    return formatOptions;
  }
  try {
    const discovery = await api.json("/api/v1");
    if (Array.isArray(discovery.formats) && discovery.formats.length > 0) {
      formatOptions = discovery.formats;
      return formatOptions;
    }
  } catch (_) {
    // Fall back to the compiled schema list when discovery is unavailable.
  }
  const formatField = schemas.repository.find((field) => field.name === "format");
  formatOptions = formatField?.options || ["raw", "oci"];
  return formatOptions;
}

function repositorySchemaWithFormats(formats) {
  return schemas.repository.map((field) => {
    if (field.name !== "format") {
      return field;
    }
    return {...field, options: formats};
  });
}

async function editRepository(repository) {
  const create = !repository;
  const value = create ? cloneJSON(defaults.repository) : {
    format: repository.format,
    type: repository.type,
    allowOverwrite: repository.allowOverwrite ? "Allow" : "Deny",
    blobStore: repository.blobStore || "default",
    upstream: repository.upstream || "",
    members: repository.members || [],
    formatConfig: repository.formatConfig || {},
    endpoints: repository.endpoints || {},
  };
  const formats = await loadFormatOptions();
  openForm({
    title: create ? "Create repository" : `Edit repository · ${repository.name}`,
    description: "Proxy repositories require an upstream URL; groups require same-format members.",
    schema: repositorySchemaWithFormats(formats).filter((field) =>
      field.name !== "allowOverwrite" || create || repository.type === "hosted")
      .map((field) => field.name === "allowOverwrite" && !create
        ? {...field, options: ["Allow", "Deny"]} : field),
    value,
    mode: create ? "create" : "edit",
    saveLabel: create ? "Create" : "Save changes",
    onSave: async (request) => {
      if (request.type !== "hosted" || !request.allowOverwrite || request.allowOverwrite === "Format default") {
        delete request.allowOverwrite;
      } else {
        request.allowOverwrite = request.allowOverwrite === "Allow";
      }
      if (!create && request.upstream === value.upstream) {
        // GET deliberately hides URL userinfo and query parameters. An unchanged
        // visible URL means "retain the stored upstream" on repository PUT.
        delete request.upstream;
      }
      const requestPath = create
        ? "/api/v1/repositories"
        : apiPath("api", "v1", "repositories", repository.name);
      const saved = await api.json(requestPath, {method: create ? "POST" : "PUT", body: request});
      const name = create ? saved.name : repository.name;
      navigateOrRefresh(routePath("repositories", name));
      showNotice(`Repository ${name} was saved.`);
    },
    onDelete: create ? null : async () => {
      await api.json(apiPath("api", "v1", "repositories", repository.name), {method: "DELETE"});
      showNotice(`Repository ${repository.name} was deleted.`);
      window.location.assign(routePath("repositories"));
    },
    deleteMessage: `Delete repository ${repository?.name} and all of its asset metadata?`,
  });
}

const settingDescriptions = {
  classification: "Ingest-time labelling: for each asset, every rule whose predicates match sets " +
    "classification.<key>=<value>; a later rule overwrites the same key, and an asset matched by no rule gets " +
    "no classification.* labels. Saving also re-labels existing assets. The labels are usable in search and in " +
    "cleanup or download-gate predicates (e.g. classification.stage).",
  "download-gate": "Blocks downloads of assets that do not match every predicate over their projected " +
    "attributes (e.g. a classification.<key> label or a scanner's scan.status). Cryptographic signature checks " +
    "are configured separately under Trust policy.",
  "trust-policy": "Verifies artifact signatures and provenance (audit records only; verify-on-push blocks " +
    "unsigned uploads; verify-on-pull blocks unverified downloads). It produces the provenance.* attributes " +
    "that a Download gate can then require.",
};

async function editRepositorySetting(repository, setting) {
  const schemaBySetting = {
    classification: schemas.classification,
    "download-gate": schemas.downloadGate,
    "trust-policy": schemas.trustPolicy,
  };
  const defaultsBySetting = {
    classification: {rules: [], inheritGlobal: true},
    "download-gate": {
      criteria: [{path: "scan.status", op: "=", value: "passed"}],
      enabled: true,
      inheritGlobal: true,
    },
    "trust-policy": {
      mode: "audit",
      publicKeys: [],
      certificateAuthorities: [],
      allowedIdentities: [],
      deniedFingerprints: [],
    },
  };
  const requestPath = apiPath("api", "v1", "repositories", repository.name, setting);
  let current = defaultsBySetting[setting];
  let exists = false;
  try {
    current = await api.json(requestPath);
    exists = true;
  } catch (error) {
    if (!(error instanceof APIError && error.status === 404)) {
      throw error;
    }
  }
  const value = cloneJSON(current);
  delete value.repository;
  delete value.updatedAt;
  delete value.managed;
  if (current.managed) {
    showNotice(
      `${humanize(setting)} is managed by declarative provisioning and cannot be edited here.`,
      true,
    );
    return;
  }
  openForm({
    title: `${humanize(setting)} · ${repository.name}`,
    description: settingDescriptions[setting],
    schema: schemaBySetting[setting],
    value,
    onSave: async (request) => {
      const result = await api.json(requestPath, {method: "PUT", body: request});
      const suffix = result?.reclassifiedAssets !== undefined
        ? ` ${result.reclassifiedAssets} assets reclassified.`
        : "";
      showNotice(`${humanize(setting)} saved.${suffix}`);
    },
    onDelete: exists ? async () => {
      await api.json(requestPath, {method: "DELETE"});
      showNotice(`${humanize(setting)} removed.`);
    } : null,
    deleteMessage: `Remove ${humanize(setting).toLowerCase()} from ${repository.name}?`,
  });
}

function uploadPanel(repositoryName) {
  const panel = element("div", "panel");
  panel.append(element("h2", "", "Upload Raw asset"));
  const form = element("form", "inline-form");
  const pathLabel = element("label", "", "Asset path");
  const pathInput = element("input");
  pathInput.required = true;
  pathInput.placeholder = "releases/application.tar.gz";
  pathLabel.append(pathInput);
  const fileLabel = element("label", "", "File");
  const fileInput = element("input");
  fileInput.type = "file";
  fileInput.required = true;
  fileLabel.append(fileInput);
  const submit = element("button", "", "Upload");
  submit.type = "submit";
  form.append(pathLabel, fileLabel, submit);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      const file = fileInput.files[0];
      if (!file) {
        showNotice("Choose a file to upload.", true);
        return;
      }
      const segments = pathInput.value.split("/");
      // Fetch normalizes dot segments before the server can validate the path.
      // Keep the same literal-path contract as the Raw API and CLI.
      if (segments.some((segment) => !segment || segment === "." || segment === ".." ||
          /[\\\x00-\x1f\x7f]/.test(segment))) {
        throw new Error("Asset path must have nonempty segments without . or .., backslashes, or control characters.");
      }
      const encodedPath = segments.map(encodeURIComponent).join("/");
      await api.upload(`/repository/${encodeURIComponent(repositoryName)}/${encodedPath}`, file);
      showNotice(`Uploaded ${pathInput.value}.`);
      form.reset();
    } catch (error) {
      showNotice(`Upload failed: ${error.message}`, true);
    }
  });
  panel.append(form);
  return panel;
}

async function renderAssets(view, repositoryName, identity) {
  view.replaceChildren(heading(
    `Assets · ${repositoryName}`,
    "Lazy-loaded repository content. Select an asset for metadata and actions.",
  ));
  const filter = element("form", "filter-bar");
  const prefix = element("input");
  prefix.placeholder = "Path prefix";
  const apply = element("button", "", "Filter");
  apply.type = "submit";
  filter.append(prefix, apply);
  const panel = element("div", "panel");
  const table = element("div");
  const controls = element("div");
  panel.append(filter, table, controls);
  view.append(panel);
  const pageSize = 50;
  const history = cursorPageHistory();
  const load = async (index) => {
    const query = queryString({prefix: prefix.value.trim()});
    const collection = `${apiPath("api", "v1", "repositories", repositoryName, "assets")}${query ? `?${query}` : ""}`;
    const page = await api.page(collection, {limit: pageSize, cursor: history.cursor(index)});
    history.record(index, page.nextCursor);
    renderTable(table, assetColumns(), page.items, {
      empty: "No matching assets.",
      href: (asset) => routePath("repositories", repositoryName, "assets", asset.id),
    });
    controls.replaceChildren(cursorPager(index, Boolean(page.nextCursor), load));
  };
  filter.addEventListener("submit", (event) => {
    event.preventDefault();
    history.reset();
    load(0).catch((error) => showNotice(error.message, true));
  });
  await load(0);
}

function assetColumns() {
  return [
    {label: "Path", render: (asset) => code(asset.path)},
    {label: "Kind", key: "kind"},
    {label: "Size", render: (asset) => formatBytes(asset.size)},
    {label: "Class", render: (asset) => {
      const classification = asset.attributes?.classification;
      if (!classification || typeof classification !== "object") {
        return "—";
      }
      const labels = Object.entries(classification).map(([key, value]) => `${key}=${value}`);
      return labels.length ? labels.join(", ") : "—";
    }},
    {label: "Provenance", render: (asset) => badge(asset.attributes?.provenance?.status || "unverified")},
    {label: "Updated", render: (asset) => formatDate(asset.updatedAt)},
  ];
}

async function renderAssetDetail(view, repositoryName, assetID, identity) {
  const requestPath = apiPath("api", "v1", "repositories", repositoryName, "assets", assetID);
  const asset = await api.json(requestPath);
  const repository = await api.json(apiPath("api", "v1", "repositories", repositoryName));
  const canMutate = repository.type !== "group";
  const actions = [button("Download", () => downloadAsset(asset))];
  if (canMutate && hasPrivilege(identity, `repository:${repositoryName}:annotate`)) {
    await loadFormatOptions();
    actions.push(button("Set attributes", () => editAttributes(asset), "secondary"));
    actions.push(button("Verify", () => verifyAsset(asset), "secondary"));
  }
  if (canMutate && hasPrivilege(identity, `repository:${repositoryName}:delete`)) {
    actions.push(button("Delete", () => deleteAsset(asset), "danger"));
  }
  view.replaceChildren(heading(`Asset · ${asset.path}`, `Repository ${repositoryName}`, actions));
  const metadata = element("div", "panel detail-panel");
  metadata.append(definitionList([
    ["ID", code(asset.id)], ["Path", code(asset.path)], ["Digest", code(asset.digest)],
    ["Size", formatBytes(asset.size)], ["Content type", asset.contentType], ["Kind", asset.kind],
    ["Reference", asset.reference], ["Subject digest", asset.subjectDigest],
    ["Created", formatDate(asset.createdAt)], ["Updated", formatDate(asset.updatedAt)],
    ["Last accessed", formatDate(asset.lastAccessed)],
  ]));
  const attributes = element("div", "panel");
  attributes.append(element("h2", "", "Attributes and provenance"));
  const pre = element("pre", "json-preview", prettyJSON(asset.attributes || {}));
  attributes.append(pre);
  if (canMutate && hasPrivilege(identity, `repository:${repositoryName}:annotate`)) {
    const mutable = Object.keys(asset.attributes || {}).filter((name) => !reservedNamespace(name));
    const actionBar = element("div", "button-row");
    for (const namespace of mutable) {
      actionBar.append(button(`Delete ${namespace}`, () => deleteAttribute(asset, namespace), "danger small"));
    }
    attributes.append(actionBar);
  }
  view.append(metadata, attributes);
  if (asset.kind === "oci-manifest") {
    const manifestPanel = element("div", "panel");
    manifestPanel.append(element("h2", "", "Manifest contents"));
    view.append(manifestPanel);
    try {
      const contents = await api.json(
        `${apiPath("api", "v1", "repositories", repositoryName, "assets", asset.id)}/manifest`,
      );
      renderManifestContents(manifestPanel, contents);
    } catch (error) {
      manifestPanel.append(element("div", "empty-state", `Could not read manifest: ${error.message}`));
    }
  }
}

function renderManifestContents(container, contents) {
  const summary = definitionList([
    ["Media type", contents.mediaType || "—"],
    ["Artifact type", contents.artifactType || "—"],
  ]);
  container.append(summary);
  // A manifest list / image index references child manifests rather than layers.
  if (contents.manifests && contents.manifests.length) {
    container.append(element("h3", "", "Manifests"));
    container.append(descriptorTable(contents.manifests));
  }
  if (contents.config) {
    container.append(element("h3", "", "Config"));
    container.append(descriptorTable([contents.config]));
  }
  if (contents.layers && contents.layers.length) {
    container.append(element("h3", "", "Layers"));
    container.append(descriptorTable(contents.layers));
  }
  if (contents.subject) {
    container.append(element("h3", "", "Subject"));
    container.append(descriptorTable([contents.subject]));
  }
  if (!contents.config && !(contents.layers || []).length && !(contents.manifests || []).length) {
    container.append(element("div", "empty-state", "This manifest references no config, layers, or child manifests."));
  }
}

function descriptorTable(descriptors) {
  const table = element("div");
  renderTable(table, [
    {label: "Media type", render: (descriptor) => descriptor.mediaType || "—"},
    {label: "Digest", render: (descriptor) => code(shortDigest(descriptor.digest))},
    {label: "Size", render: (descriptor) => formatBytes(descriptor.size)},
  ], descriptors, {empty: "None."});
  return table;
}

function editAttributes(asset) {
  openForm({
    title: `Set attributes · ${asset.path}`,
    description: "System, classification, provenance, and installed format namespaces (including dotted children) are read-only.",
    schema: schemas.attributes,
    value: {namespace: "scan", value: asset.attributes?.scan || {status: "passed"}},
    onSave: async (request) => {
      if (reservedNamespace(request.namespace)) {
        throw new Error("That namespace is managed by the server.");
      }
      try {
        await api.json(attributePath(asset, request.namespace), {
          method: "PUT", body: request.value, headers: assetGenerationHeaders(asset),
        });
      } catch (error) {
        if (refreshAfterStaleAsset(error)) {
          return;
        }
        throw error;
      }
      showNotice(`Attribute namespace ${request.namespace} was saved.`);
      window.location.reload();
    },
  });
}

async function deleteAttribute(asset, namespace) {
  if (!await confirmAction(`Delete attribute namespace ${namespace} from ${asset.path}?`)) {
    return;
  }
  try {
    await api.json(attributePath(asset, namespace), {
      method: "DELETE", headers: assetGenerationHeaders(asset),
    });
  } catch (error) {
    if (refreshAfterStaleAsset(error)) {
      return;
    }
    throw error;
  }
  showNotice(`Attribute namespace ${namespace} was deleted.`);
  window.location.reload();
}

function assetGenerationHeaders(asset) {
  return {"If-Match": `"${asset.digest}"`};
}

function refreshAfterStaleAsset(error) {
  if (!(error instanceof APIError && error.status === 412)) {
    return false;
  }
  refreshCurrentRoute();
  showNotice("The asset content changed. Its details are being refreshed; retry the annotation on the current artifact.", true);
  return true;
}

function attributePath(asset, namespace) {
  return apiPath("api", "v1", "repositories", asset.repository, "assets", asset.id, "attributes", namespace);
}

function reservedNamespace(namespace) {
  const root = namespace.split(".", 1)[0];
  const formats = formatOptions || schemas.repository.find((field) => field.name === "format").options;
  return ["sys", "raw", "oci", "classification", "provenance", ...formats].includes(root);
}

function verifyAsset(asset) {
  openForm({
    title: `Verify · ${asset.path}`,
    description: "Submit a detached signature, certificate chain, payload, or DSSE envelope in Advanced JSON.",
    schema: schemas.verification,
    value: {signature: "", payload: "", certificate: "", certificateChain: []},
    saveLabel: "Verify",
    onSave: async (request) => {
      let result;
      try {
        result = await api.json(
          apiPath(
            "api", "v1", "repositories", asset.repository,
            "assets", asset.id, "verification",
          ),
          {method: "POST", body: request},
        );
      } catch (error) {
        if (!(error instanceof APIError && error.status === 422 && error.payload?.status)) {
          throw error;
        }
        result = error.payload;
      }
      window.dispatchEvent(new CustomEvent("suxen:resource-changed"));
      const evidence = result.reason || result.identity || result.fingerprint || "complete";
      showNotice(`Verification ${result.status}: ${evidence}.`, result.status !== "passed");
    },
  });
}

async function downloadAsset(asset) {
  const publicPath = asset.formatPath || asset.path;
  const encodedPath = publicPath.split("/").map(encodeURIComponent).join("/");
  const downloadPath = asset.formatPath && asset.formatPath !== asset.path
    ? apiPath("api", "v1", "repositories", asset.repository, "assets", asset.id, "download")
    : `/repository/${encodeURIComponent(asset.repository)}/${encodedPath}`;
  try {
    await api.download(downloadPath, publicPath.split("/").pop());
  } catch (error) {
    if (error instanceof APIError && isDownloadGated(error)) {
      showNotice(
        `Download blocked by this repository's download gate: ${asset.path} does not satisfy its ` +
        "required predicates. Adjust the gate or the asset's attributes to allow it.",
        true,
      );
      return;
    }
    throw error;
  }
}

function isDownloadGated(error) {
  if (error.payload?.code === "download_gated") {
    return true;
  }
  const marker = error.payload?.type || error.payload?.detail || error.message || "";
  return String(marker).includes("download_gated");
}

async function deleteAsset(asset) {
  if (!await confirmAction(`Delete asset ${asset.path}? Blob bytes are reclaimed later by GC.`)) {
    return;
  }
  await api.json(apiPath("api", "v1", "repositories", asset.repository, "assets", asset.id), {method: "DELETE"});
  showNotice(`Asset ${asset.path} was deleted.`);
  window.location.assign(routePath("repositories", asset.repository, "assets"));
}

async function chooseCleanupPolicy(repositoryName) {
  const policies = await api.collection("/api/v1/cleanup-policies");
  const attached = policies.filter((policy) => (policy.repositories || []).includes(repositoryName));
  if (attached.length === 0) {
    showNotice(
      `No cleanup policies are attached to ${repositoryName}. Attach one under Cleanup policies first.`,
      true,
    );
    return;
  }
  openForm({
    title: `Preview cleanup · ${repositoryName}`,
    description: "Choose a policy. The next page shows the dry-run deletion list before apply.",
    schema: [{
      name: "policy",
      label: "Cleanup policy",
      type: "select",
      required: true,
      options: attached.map((policy) => policy.name),
    }],
    value: {policy: attached[0].name},
    saveLabel: "Preview",
    onSave: async ({policy}) => {
      const query = new URLSearchParams({policy});
      window.location.assign(`${routePath("repositories", repositoryName, "cleanup")}?${query}`);
    },
  });
}

async function renderCleanupPreview(view, repositoryName, query, identity) {
  const policy = query.get("policy");
  if (!policy) {
    await chooseCleanupPolicy(repositoryName);
    return;
  }
  const request = queryString({policy, dryRun: true});
  const requestPath = apiPath("api", "v1", "repositories", repositoryName, "cleanup");
  const task = await api.json(`${requestPath}?${request}`, {method: "POST"});
  const result = task.result || {};
  const apply = hasPrivilege(identity, `repository:${repositoryName}:delete`)
    ? button("Apply this cleanup", () => applyCleanup(repositoryName, policy), "danger")
    : null;
  view.replaceChildren(heading(
    `Cleanup preview · ${repositoryName}`,
    `Policy ${policy}: ${result.matched || 0} matching assets.`,
    [apply],
  ));
  const summary = element("div", "panel");
  summary.append(definitionList([
    ["Scanned", result.scanned], ["Matched", result.matched],
    ["Would delete", result.wouldDelete?.length || 0], ["Task", routeLink(`#${task.id}`, routePath("tasks", task.id))],
  ]));
  const table = element("div", "panel");
  renderTable(table, [
    {
      label: "Candidate",
      render: (candidate) => code(
        typeof candidate === "string" ? candidate : candidate.path || candidate.id,
      ),
    },
    {label: "Details", render: (candidate) => typeof candidate === "string" ? "—" : prettyJSON(candidate)},
  ], result.wouldDelete || [], {empty: "Nothing would be deleted."});
  view.append(summary, table);
}

async function applyCleanup(repositoryName, policy) {
  if (!await confirmAction(`Apply cleanup policy ${policy} to ${repositoryName}?`, {label: "Apply cleanup"})) {
    return;
  }
  const query = queryString({policy, dryRun: false});
  const requestPath = apiPath("api", "v1", "repositories", repositoryName, "cleanup");
  const task = await api.json(`${requestPath}?${query}`, {method: "POST"});
  showNotice(`Cleanup task ${task.id} completed with status ${task.status}.`);
  window.location.assign(routePath("tasks", task.id));
}

async function renderBrowse(view, repositoryName, query) {
  if (!repositoryName) {
    view.replaceChildren(heading(
      "Browse repositories",
      "Select a readable repository to explore components and assets.",
    ));
    const panel = element("div", "panel");
    const table = element("div");
    const controls = element("div");
    panel.append(table, controls);
    view.append(panel);
    const pageSize = 50;
    const history = cursorPageHistory();
    const load = async (index) => {
      const page = await api.page("/api/v1/browse", {limit: pageSize, cursor: history.cursor(index)});
      history.record(index, page.nextCursor);
      renderTable(table, [
        {label: "Name", render: (repository) => code(repository.name)},
        {label: "Format", key: "format"},
        {label: "Type", render: (repository) => badge(repository.type)},
      ], page.items, {href: (repository) => routePath("browse", repository.name)});
      controls.replaceChildren(cursorPager(index, Boolean(page.nextCursor), load));
    };
    await load(0);
    return;
  }
  const repository = await api.json(apiPath("api", "v1", "repositories", repositoryName));
  if (repository.format === "oci") {
    view.replaceChildren(heading(
      `Browse · ${repositoryName}`,
      "Images and their tags. Select a tag to inspect its manifest config and layers.",
    ));
    await renderOCIComponents(view, repositoryName);
    return;
  }
  const componentFilter = query.get("component") || "";
  view.replaceChildren(heading(
    `Browse · ${repositoryName}`,
    "Components and versions are grouped from repository assets.",
  ));
  const panel = element("div", "panel");
  const tree = element("div", "browse-tree");
  const controls = element("div");
  panel.append(tree, controls);
  view.append(panel);
  const pageSize = 50;
  const history = cursorPageHistory();
  const load = async (index) => {
    const filter = queryString({component: componentFilter});
    const path = `${apiPath("api", "v1", "repositories", repositoryName, "browse")}${filter ? `?${filter}` : ""}`;
    const page = await api.page(path, {limit: pageSize, cursor: history.cursor(index)});
    history.record(index, page.nextCursor);
    renderBrowseTree(tree, page.items);
    controls.replaceChildren(cursorPager(index, Boolean(page.nextCursor), load));
  };
  await load(0);
}

// renderOCIComponents groups each bounded page of tags/digests by image name;
// layer content is reached by opening a manifest.
async function renderOCIComponents(view, repositoryName) {
  const panel = element("div", "panel");
  const list = element("div", "browse-tree");
  const controls = element("div");
  panel.append(list, controls);
  view.append(panel);
  const pageSize = 50;
  const history = cursorPageHistory();
  const load = async (index) => {
    const path = apiPath("api", "v1", "repositories", repositoryName, "components");
    const page = await api.page(path, {limit: pageSize, cursor: history.cursor(index)});
    history.record(index, page.nextCursor);
    renderOCIComponentGroups(list, repositoryName, page.items, Boolean(page.nextCursor));
    controls.replaceChildren(cursorPager(index, Boolean(page.nextCursor), load));
  };
  await load(0);
}

function renderOCIComponentGroups(container, repositoryName, versions, hasNext) {
  container.replaceChildren();
  if (!versions.length) {
    container.append(element("div", "empty-state", hasNext
      ? "No images on this page. Continue to the next page."
      : "No images found."));
    return;
  }
  const groups = new Map();
  for (const version of versions) {
    if (!groups.has(version.component)) {
      groups.set(version.component, []);
    }
    groups.get(version.component).push(version);
  }
  for (const [component, items] of groups) {
    const details = element("details", "tree-node");
    details.open = groups.size <= 3;
    const summary = element("summary");
    summary.append(code(component), element("span", "muted", ` · ${items.length} on this page`));
    details.append(summary);
    const table = element("div");
    renderTable(table, [
      {label: "Tag / digest", render: (version) => isDigestReference(version)
        ? code(shortDigest(version.reference))
        : code(version.version)},
      {label: "Type", render: (version) => badge(isDigestReference(version) ? "digest" : "tag")},
      {label: "Digest", render: (version) => code(shortDigest(version.digest))},
      {label: "Size", render: (version) => formatBytes(version.size)},
      {label: "Pushed", render: (version) => formatDate(version.updatedAt)},
    ], items, {
      empty: "No versions.",
      href: (version) => routePath("repositories", repositoryName, "assets", version.assetId),
    });
    details.append(table);
    container.append(details);
  }
}

function isDigestReference(version) {
  return typeof version.reference === "string" && version.reference.startsWith("sha256:");
}

// shortDigest trims a "sha256:<64 hex>" digest to a readable prefix for tables.
function shortDigest(digest) {
  if (typeof digest !== "string") {
    return "—";
  }
  const [algorithm, hex] = digest.split(":");
  if (!hex) {
    return digest;
  }
  return `${algorithm}:${hex.slice(0, 12)}`;
}

function renderBrowseTree(container, rows) {
  container.replaceChildren();
  if (!rows.length) {
    container.append(element("div", "empty-state", "No components found."));
    return;
  }
  const groups = new Map();
  for (const row of rows) {
    const component = row.component || "(root)";
    const items = groups.get(component) || [];
    items.push(row);
    groups.set(component, items);
  }
  for (const [component, items] of groups) {
    const details = element("details", "tree-node");
    details.open = groups.size === 1;
    details.append(element("summary", "", `${component} (${items.length})`));
    const list = element("ul");
    for (const item of items) {
      const label = item.version ? `${item.version} · ${item.asset.path}` : item.asset.path;
      const link = routeLink(label, routePath("repositories", item.repository, "assets", item.asset.id));
      const row = element("li");
      row.append(link);
      list.append(row);
    }
    details.append(list);
    container.append(details);
  }
}

async function renderSearch(view, query) {
  view.replaceChildren(heading(
    "Global search",
    "Search readable repositories by text, path, classification, or attribute.",
  ));
  const form = element("form", "panel search-form");
  const q = searchInput("Text", "q", query.get("q") || "");
  const prefix = searchInput("Path prefix", "pathPrefix", query.get("pathPrefix") || "");
  const classification = searchInput("Classification", "classification", query.get("classification") || "");
  const repositories = searchInput("Repositories", "repository", query.getAll("repository").join(", "));
  const attributes = searchInput("Attributes", "attribute", query.getAll("attribute").join(", "));
  const submit = element("button", "", "Search");
  submit.type = "submit";
  form.append(q.label, prefix.label, classification.label, repositories.label, attributes.label, submit);
  const resultsPanel = element("div", "panel");
  const results = element("div");
  const controls = element("div");
  resultsPanel.append(results, controls);
  view.append(form, resultsPanel);
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    const values = new URLSearchParams();
    addSearchValue(values, "q", q.input.value);
    addSearchValue(values, "pathPrefix", prefix.input.value);
    addSearchValue(values, "classification", classification.input.value);
    for (const repository of splitComma(repositories.input.value)) {
      values.append("repository", repository);
    }
    for (const attribute of splitComma(attributes.input.value)) {
      values.append("attribute", attribute);
    }
    window.location.assign(`${routePath("search")}?${values}`);
  });
  if (![...query.keys()].length) {
    results.append(element("div", "empty-state", "Enter search filters to begin."));
    return;
  }
  const pageSize = 50;
  const history = cursorPageHistory();
  const load = async (index) => {
    const request = new URLSearchParams(query);
    request.delete("cursor");
    request.delete("page");
    const page = await api.page(`/api/v1/search?${request}`, {limit: pageSize, cursor: history.cursor(index)});
    history.record(index, page.nextCursor);
    renderTable(results, [
      {label: "Repository", render: (item) => code(item.repository)},
      {label: "Component", key: "component"},
      {label: "Version", key: "version"},
      {label: "Path", render: (item) => code(item.asset.path)},
      {label: "Size", render: (item) => formatBytes(item.asset.size)},
    ], page.items, {
      empty: page.nextCursor ? "No matches on this page. Continue to scan." : "No assets matched.",
      href: (item) => routePath("repositories", item.repository, "assets", item.asset.id),
    });
    controls.replaceChildren(cursorPager(index, Boolean(page.nextCursor), load));
  };
  await load(0);
}

function searchInput(labelText, name, value) {
  const label = element("label", "", labelText);
  const input = element("input");
  input.name = name;
  input.value = value;
  label.append(input);
  return {label, input};
}

function addSearchValue(query, name, value) {
  if (value.trim()) {
    query.set(name, value.trim());
  }
}

function splitComma(value) {
  return value.split(",").map((item) => item.trim()).filter(Boolean);
}

function humanize(value) {
  return value.replaceAll("-", " ").replace(/^./, (letter) => letter.toUpperCase());
}
