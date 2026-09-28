"use strict";

import {api, apiPath, queryString} from "./api.js";
import {cloneJSON, isExactJSONNumber} from "./json_codec.js";
import {
  badge,
  button,
  code,
  confirmAction,
  definitionList,
  element,
  formatBytes,
  formatDate,
  heading,
  openForm,
  numberedPager,
  pager,
  prettyJSON,
  renderTable,
  routeLink,
  showNotice,
  showSecret,
} from "./components.js";
import {canLoadPrivilegeCatalog, hasPrivilege, routePath} from "./router.js";
import {resourceRouteKind} from "./route_shapes.js";
import {defaults, schemas} from "./schemas.js";
import {normalizePage} from "./pagination.js";
import {navigateOrRefresh, refreshCurrentRoute} from "./navigation.js";
import {startOIDCLogin} from "./session.js";

const resources = {
  "blob-stores": {
    singular: "blob store",
    title: "Blob stores",
    description: "Named filesystem and S3-compatible storage backends.",
    schema: schemas.blobStore,
    defaults: defaults.blobStore,
    collection: "/api/v1/blob-stores",
    key: "name",
    privilege: "admin:blob-stores:write",
    columns: [
      {label: "Name", render: (item) => code(item.name)},
      {label: "Driver", key: "driver"},
      {label: "Configuration", render: (item) => referenceLabel(item.configurationRef)},
      {label: "Created", render: (item) => formatDate(item.createdAt)},
    ],
  },
  "cleanup-policies": {
    singular: "cleanup policy",
    title: "Cleanup policies",
    description: "Reusable retention rules. Repository details expose preview and apply actions.",
    schema: schemas.cleanupPolicy,
    defaults: defaults.cleanupPolicy,
    collection: "/api/v1/cleanup-policies",
    key: "name",
    privilege: "admin:cleanup-policies:write",
    columns: [
      {label: "Name", render: (item) => code(item.name)},
      {label: "Repositories", render: (item) => item.repositories?.join(", ") || "—"},
      {label: "Criteria", render: (item) => predicateSummary(item.criteria)},
      {label: "Status", render: (item) => badge(item.enabled ? "enabled" : "disabled")},
      {label: "Keep last", key: "keepLast"},
      {label: "Updated", render: (item) => formatDate(item.updatedAt)},
    ],
  },
  users: {
    singular: "user",
    title: "Users",
    description: "Local accounts, role assignments, and scoped API tokens.",
    schema: schemas.user,
    defaults: defaults.user,
    collection: "/api/v1/users",
    key: "username",
    privilege: "admin:users:write",
    columns: [
      {label: "Username", render: (item) => code(item.username)},
      {label: "Administrator", render: (item) => badge(item.admin ? "enabled" : "disabled")},
      {label: "Created", render: (item) => formatDate(item.createdAt)},
    ],
  },
  roles: {
    singular: "role",
    title: "Roles",
    description: "Named privilege bundles assigned to users.",
    schema: schemas.role,
    defaults: defaults.role,
    collection: "/api/v1/roles",
    key: "name",
    privilege: "admin:roles:write",
    columns: [
      {label: "Name", render: (item) => code(item.name)},
      {label: "Description", key: "description"},
      {label: "Privileges", render: (item) => item.privileges?.join(", ") || "—"},
    ],
  },
  "oidc-providers": {
    singular: "OIDC provider",
    title: "OIDC providers",
    description: "External issuers and group-to-role mappings.",
    schema: schemas.oidc,
    defaults: defaults.oidc,
    collection: "/api/v1/oidc-providers",
    key: "name",
    privilege: "admin:oidc-providers:write",
    columns: [
      {label: "Name", render: (item) => code(item.name)},
      {label: "Issuer", render: (item) => code(item.issuer)},
      {label: "Client ID", key: "clientId"},
      {label: "Default roles", render: (item) => item.defaultRoles?.join(", ") || "—"},
    ],
  },
  webhooks: {
    singular: "webhook",
    title: "Webhooks",
    description: "Durable outbound subscriptions and delivery history.",
    schema: schemas.webhook,
    defaults: defaults.webhook,
    collection: "/api/v1/webhooks",
    key: "name",
    privilege: "admin:webhooks:write",
    columns: [
      {label: "Name", key: "name"},
      {label: "URL", render: (item) => code(item.url)},
      {label: "Events", render: (item) => item.events?.join(", ") || "—"},
      {label: "Status", render: (item) => badge(item.enabled ? "enabled" : "disabled")},
    ],
  },
};

export async function renderResourceRoute(view, route, identity) {
  const [resourceName, identifier] = route.segments;
  const descriptor = resources[resourceName];
  const routeKind = resourceRouteKind(route.segments);
  if (!descriptor || !routeKind) {
    return false;
  }
  if (routeKind === "resource-list") {
    await renderList(view, resourceName, descriptor, identity);
    return true;
  }
  if (routeKind === "tokens") {
    await renderTokens(view, identifier, identity);
    return true;
  }
  if (routeKind === "deliveries") {
    await renderDeliveries(view, identifier);
    return true;
  }
  await renderDetail(view, resourceName, descriptor, identifier, identity);
  return true;
}

async function renderList(view, resourceName, descriptor, identity) {
  const canWrite = hasPrivilege(identity, descriptor.privilege);
  const create = canWrite ? button(`Create ${descriptor.singular}`, () => editResource(
    resourceName,
    descriptor,
    {...descriptor.defaults},
    "create",
  )) : null;
  view.replaceChildren(heading(descriptor.title, descriptor.description, [create]));
  const panel = element("div", "panel");
  const table = element("div");
  const pageControls = element("div");
  panel.append(table, pageControls);
  view.append(panel);

  const pageSize = 50;
  const load = async (requestedPage) => {
    let page;
    if (resourceName === "blob-stores") {
      const query = queryString({limit: pageSize, page: requestedPage});
      const response = await api.json(`${descriptor.collection}?${query}`);
      page = normalizePage(response);
    } else {
      page = await api.page(descriptor.collection, {limit: pageSize, page: requestedPage});
    }
    const columns = [
      ...descriptor.columns,
      {
        label: "Ownership",
        render: (item) => item.managed ? badge("managed") : "manual",
      },
    ];
    renderTable(table, columns, page.items, {
      empty: `No ${descriptor.title.toLowerCase()} are configured.`,
      href: (item) => routePath(resourceName, item[descriptor.key]),
    });
    pageControls.replaceChildren(numberedPager(requestedPage, page.total || 0, pageSize, load));
  };
  await load(1);
}

async function renderDetail(view, resourceName, descriptor, identifier, identity) {
  const item = await api.json(itemPath(descriptor, identifier));
  const canWrite = hasPrivilege(identity, descriptor.privilege);
  const immutableDefault = resourceName === "blob-stores" && identifier === "default";
  const actions = [item.managed ? badge("managed") : null];
  if (canWrite && !immutableDefault && !item.managed) {
    actions.push(button("Edit", () => editResource(resourceName, descriptor, item, "edit")));
    actions.push(button("Delete", () => deleteResource(resourceName, descriptor, item), "danger"));
  }
  if (resourceName === "users") {
    actions.push(routeLink("Tokens", routePath("users", identifier, "tokens"), "button secondary"));
  }
  if (resourceName === "webhooks") {
    actions.push(routeLink("Deliveries", routePath("webhooks", identifier, "deliveries"), "button secondary"));
  }
  if (resourceName === "oidc-providers") {
    actions.push(button("Login with provider", () => startOIDCLogin(identifier)));
    actions.push(button("Logout OIDC session", () => logoutOIDC(identifier), "secondary"));
  }
  view.replaceChildren(
    heading(`${descriptor.singular} · ${displayName(item, descriptor)}`, descriptor.description, actions),
  );
  const panel = element("div", "panel detail-panel");
  panel.append(definitionList(detailValues(item)));
  view.append(panel);
  if (resourceName === "roles") {
    await appendPrivilegeReference(view, identity);
  }
  if (resourceName === "blob-stores") {
    appendBlobStoreUsage(view, identifier);
    appendBlobStorePrune(view, identifier, identity);
  }
}

// Blob-store usage is a physical enumeration the SPI documents as expensive, so
// it is fetched only when the operator asks, not with the detail page.
function appendBlobStoreUsage(view, identifier) {
  const panel = element("section", "panel");
  panel.append(
    element("h2", "", "Usage"),
    element(
      "p",
      "",
      "Scanning enumerates the whole backend and can be slow on large stores. " +
        "Referenced bytes are still in use; unreferenced bytes are what a prune could reclaim.",
    ),
  );
  const results = element("div", "");
  const scan = button("Scan usage", async () => {
    scan.disabled = true;
    results.replaceChildren(element("div", "loading", "Scanning…"));
    try {
      const usage = await api.json(
        `/api/v1/blob-stores/${encodeURIComponent(identifier)}/usage`,
      );
      results.replaceChildren(definitionList([
        ["Objects", String(usage.objectCount)],
        ["Total", formatBytes(usage.totalBytes)],
        ["Referenced", `${usage.referencedCount} · ${formatBytes(usage.referencedBytes)}`],
        [
          "Unreferenced (reclaimable)",
          `${usage.unreferencedCount} · ${formatBytes(usage.unreferencedBytes)}`,
        ],
      ]));
    } catch (error) {
      results.replaceChildren(element("p", "inline-error", error.message));
      showNotice(`Usage scan failed: ${error.message}`, true);
    } finally {
      scan.disabled = false;
    }
  });
  panel.append(scan, results);
  view.append(panel);
}

// Pruning deletes stored data, so it is gated by admin:gc:run rather than the
// blob-store write privilege that governs the rest of this page.
function appendBlobStorePrune(view, identifier, identity) {
  if (!hasPrivilege(identity, "admin:gc:run")) {
    return;
  }
  const panel = element("section", "panel");
  panel.append(
    element("h2", "", "Prune"),
    element(
      "p",
      "",
      "Deletes blobs no metadata references from this store. Preview first; applying is irreversible.",
    ),
  );
  const form = element("form", "inline-form");
  const label = element("label", "", "Grace period");
  const grace = element("input");
  grace.name = "grace";
  grace.value = "24h";
  grace.required = true;
  label.append(grace);
  const submit = element("button", "", "Preview prune");
  submit.type = "submit";
  form.append(label, submit);
  const results = element("div");
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    previewBlobStorePrune(results, identifier, grace.value).catch((error) => {
      showNotice(`Prune preview failed: ${error.message}`, true);
    });
  });
  panel.append(form, results);
  view.append(panel);
}

async function previewBlobStorePrune(container, identifier, grace) {
  const result = await runBlobStorePrune(identifier, true, grace);
  const actionBar = element("div", "button-row");
  actionBar.append(
    button("Prune this store", () => applyBlobStorePrune(identifier, grace), "danger"),
  );
  container.replaceChildren(
    element("h3", "", "Deletion preview"),
    definitionList([
      ["Scanned", result.scanned],
      ["Referenced", result.referenced],
      ["Would delete", result.wouldDelete?.length || 0],
      ["Grace period", result.gracePeriod],
    ]),
  );
  const table = element("div", "preview-table");
  renderTable(table, [
    {label: "Unreferenced blob", render: (digest) => code(digest)},
  ], result.wouldDelete || [], {empty: "No blobs are eligible for deletion."});
  container.append(table, actionBar);
}

async function applyBlobStorePrune(identifier, grace) {
  const accepted = await confirmAction(
    `Permanently delete every unreferenced blob older than ${grace} from "${identifier}"?`,
    {title: "Prune blob store", label: "Delete eligible blobs"},
  );
  if (!accepted) {
    return;
  }
  const result = await runBlobStorePrune(identifier, false, grace);
  showNotice(
    `Pruned ${result.deleted} blobs and reclaimed ${formatBytes(result.reclaimedBytes)} from ${identifier}.`,
  );
  refreshCurrentRoute();
}

function runBlobStorePrune(identifier, dryRun, grace) {
  const query = queryString({dryRun, grace});
  return api.json(
    `${apiPath("api", "v1", "blob-stores", identifier)}/gc?${query}`,
    {method: "POST"},
  );
}

async function appendPrivilegeReference(view, identity) {
  const panel = element("section", "panel");
  panel.append(element("h2", "", "Privilege syntax"));
  if (!canLoadPrivilegeCatalog(identity)) {
    panel.append(element(
      "p",
      "",
      "The privilege catalog requires admin:privileges:read. " +
        "Role actions remain governed separately by role permissions.",
    ));
    view.append(panel);
    return;
  }
  const reference = await api.json("/api/v1/privileges");
  panel.append(
    element("p", "", reference.syntax),
    definitionList([
      ["Repository actions", reference.repositoryActions?.join(", ")],
      ["Administrative resources", reference.adminResources?.join(", ")],
    ]),
  );
  view.append(panel);
}

function detailValues(item) {
  return Object.entries(item).map(([name, value]) => [humanize(name), displayValue(value)]);
}

function displayValue(value) {
  if (isPredicateList(value)) {
    const list = element("ul", "predicate-summary");
    for (const predicate of value) {
      list.append(element("li", "", formatPredicate(predicate)));
    }
    return list;
  }
  if (Array.isArray(value)) {
    return value.length ? value.join(", ") : "—";
  }
  if (value && typeof value === "object" && !isExactJSONNumber(value)) {
    const pre = element("pre", "json-preview", prettyJSON(value));
    return pre;
  }
  if (typeof value === "boolean") {
    return badge(value ? "enabled" : "disabled");
  }
  return String(value ?? "—");
}

// Renders one conjunctive predicate as "path op value" (value omitted for
// exists/absent, comma-joined for in/not-in).
export function formatPredicate(predicate) {
  const value = Array.isArray(predicate.value)
    ? predicate.value.join(", ")
    : predicate.value;
  const parts = [predicate.path, predicate.op];
  if (value !== undefined && value !== null && value !== "") {
    parts.push(String(value));
  }
  return parts.filter((part) => part !== undefined && part !== null && part !== "").join(" ");
}

function isPredicateList(value) {
  return Array.isArray(value) && value.length > 0 &&
    value.every((entry) => entry && typeof entry === "object" && "path" in entry && "op" in entry);
}

export function predicateSummary(criteria) {
  return isPredicateList(criteria) ? criteria.map(formatPredicate).join(" · ") : "—";
}

function humanize(value) {
  return value.replace(/([A-Z])/g, " $1").replace(/^./, (letter) => letter.toUpperCase());
}

function displayName(item, descriptor) {
  return item.name || item.username || item[descriptor.key];
}

function itemPath(descriptor, identifier) {
  return `${descriptor.collection}/${encodeURIComponent(identifier)}`;
}

function editResource(resourceName, descriptor, item, mode) {
  const identifier = item[descriptor.key];
  openForm({
    title: `${mode === "create" ? "Create" : "Edit"} ${descriptor.singular}`,
    description: descriptor.description,
    schema: descriptor.schema,
    value: editableValue(resourceName, item, mode),
    mode,
    saveLabel: mode === "create" ? "Create" : "Save changes",
    onSave: async (value) => {
      const requestPath = mode === "create" ? descriptor.collection : itemPath(descriptor, identifier);
      const saved = await api.json(requestPath, {method: mode === "create" ? "POST" : "PUT", body: value});
      const savedIdentifier = mode === "create"
        ? saved?.[descriptor.key] ?? value[descriptor.key]
        : identifier;
      navigateOrRefresh(routePath(resourceName, savedIdentifier));
      showNotice(`${descriptor.singular} ${savedIdentifier} was saved.`);
    },
    onDelete: mode === "edit" ? async () => {
      await api.json(itemPath(descriptor, identifier), {method: "DELETE"});
      showNotice(`${descriptor.singular} ${identifier} was deleted.`);
      window.location.assign(routePath(resourceName));
    } : null,
    deleteMessage: `Delete ${descriptor.singular} ${displayName(item, descriptor)}?`,
  });
}

async function deleteResource(resourceName, descriptor, item) {
  const identifier = item[descriptor.key];
  if (!await confirmAction(`Delete ${descriptor.singular} ${displayName(item, descriptor)}?`)) {
    return;
  }
  await api.json(itemPath(descriptor, identifier), {method: "DELETE"});
  showNotice(`${descriptor.singular} ${identifier} was deleted.`);
  window.location.assign(routePath(resourceName));
}

const editableFields = {
  "blob-stores": ["name", "driver", "configurationRef", "attributes"],
  "cleanup-policies": ["name", "repositories", "criteria", "keepLast", "action", "enabled"],
  users: ["username", "password", "admin", "roles"],
  roles: ["name", "description", "privileges"],
  "oidc-providers": ["name", "issuer", "clientId", "clientSecret", "scopes", "groupsClaim",
    "defaultRoles", "groupRoles", "allowPasswordGrant"],
  webhooks: ["name", "url", "secret", "events", "repositories", "enabled"],
};

function editableValue(resourceName, item, mode) {
  // Preserve the complete writable request shape, including fields that a form
  // has not exposed yet, without echoing response-only lifecycle fields in PUT.
  const allowed = editableFields[resourceName];
  const value = Object.fromEntries(allowed.filter((name) => Object.hasOwn(item, name))
    .map((name) => [name, cloneJSON(item[name])]));
  if (resourceName === "user" || resourceName === "users") {
    value.password = "";
  }
  if (resourceName === "oidc-providers") {
    value.clientSecret = "";
  }
  if (resourceName === "webhooks") {
    value.secret = "";
  }
  if (mode === "edit") {
    delete value.name;
    delete value.username;
    delete value.id;
  }
  return value;
}

async function renderTokens(view, username, identity) {
  const canWrite = hasPrivilege(identity, "admin:users:write");
  const collection = apiPath("api", "v1", "users", username, "tokens");
  view.replaceChildren(heading(
    `Tokens · ${username}`,
    "Scoped API credentials. Secret values are shown once.",
    [canWrite ? button("Create token", () => createToken(username, collection)) : null],
  ));
  const panel = element("div", "panel");
  const table = element("div");
  const controls = element("div");
  panel.append(table, controls);
  view.append(panel);
  const pageSize = 50;
  const load = async (requestedPage) => {
    const page = await api.page(collection, {limit: pageSize, page: requestedPage});
    renderTable(table, [
      {label: "ID", render: (token) => code(token.id)},
      {label: "Name", key: "name"},
      {label: "Scopes", render: (token) => token.scopes?.join(", ") || "Role privileges"},
      {label: "Created", render: (token) => formatDate(token.createdAt)},
      {
        label: "Actions",
        render: (token) => canWrite
          ? button("Revoke", () => revokeToken(collection, token), "danger small")
          : "—",
      },
    ], page.items, {empty: "No API tokens exist for this user."});
    controls.replaceChildren(numberedPager(requestedPage, page.total || 0, pageSize, load));
  };
  await load(1);
}

function createToken(username, collection) {
  openForm({
    title: `Create token · ${username}`,
    schema: schemas.token,
    value: {name: "api", scopes: []},
    mode: "create",
    saveLabel: "Create token",
    onSave: async (request) => {
      const created = await api.json(collection, {method: "POST", body: request});
      showSecret(`Token for ${username}`, created.token);
      refreshCurrentRoute();
    },
  });
}

async function revokeToken(collection, token) {
  if (!await confirmAction(`Revoke token ${token.name} (#${token.id})?`)) {
    return;
  }
  await api.json(`${collection}/${token.id}`, {method: "DELETE"});
  refreshCurrentRoute();
  showNotice(`Token ${token.id} was revoked.`);
}

async function renderDeliveries(view, webhookName) {
  view.replaceChildren(heading(
    `Webhook deliveries · ${webhookName}`,
    "Delivery history is read-only. The server does not currently expose ping or requeue actions.",
  ));
  const panel = element("div", "panel");
  const table = element("div");
  const controls = element("div");
  panel.append(table, controls);
  view.append(panel);
  const collection = apiPath("api", "v1", "webhooks", webhookName, "deliveries");
  let deliveries = [];
  let cursor = "";
  const load = async () => {
    const page = await api.page(collection, {limit: 50, cursor});
    deliveries.push(...page.items);
    cursor = page.nextCursor || "";
    renderTable(table, [
      {label: "ID", render: (delivery) => code(delivery.id)},
      {label: "Event", key: "event"},
      {label: "Repository", key: "repository"},
      {label: "Status", render: (delivery) => badge(delivery.status)},
      {label: "Attempts", key: "attempts"},
      {label: "Next attempt", render: (delivery) => formatDate(delivery.nextAttemptAt)},
      {label: "Last error", key: "lastError"},
    ], deliveries, {empty: "This webhook has no delivery history."});
    controls.replaceChildren(pager(load, Boolean(cursor)));
  };
  await load();
}

function referenceLabel(reference) {
  if (!reference) {
    return "—";
  }
  return reference.env ? `env:${reference.env}` : `file:${reference.file}`;
}

async function logoutOIDC(provider) {
  let logoutError = null;
  try {
    await api.json(`/auth/oidc/${encodeURIComponent(provider)}/logout`, {method: "POST"});
  } catch (error) {
    logoutError = error;
  }
  window.dispatchEvent(new CustomEvent("suxen:session-ended", {
    detail: {provider, remoteSucceeded: !logoutError},
  }));
  if (logoutError) {
    throw logoutError;
  }
}
