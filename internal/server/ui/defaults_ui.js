"use strict";

import {api} from "./api.js";
import {
  badge,
  button,
  confirmAction,
  definitionList,
  element,
  heading,
  openForm,
  showNotice,
} from "./components.js";
import {refreshCurrentRoute} from "./navigation.js";
import {predicateSummary} from "./resource_views.js";
import {cloneJSON} from "./json_codec.js";
import {hasPrivilege} from "./router.js";
import {schemas} from "./schemas.js";

// The three instance-wide policy defaults are singletons, not collections, so
// they get a dedicated page instead of the resource list/detail machinery.
const panels = [
  {
    title: "Download gate",
    description:
      "Predicates every inheriting repository ANDs onto its own before a download is allowed.",
    path: "/api/v1/download-gate-defaults",
    read: "admin:download-gate-defaults:read",
    write: "admin:download-gate-defaults:write",
    schema: schemas.downloadGateDefaults,
    blank: {criteria: [], enabled: false},
    summary: (gate) => definitionList([
      ["Status", badge(gate.enabled ? "enabled" : "disabled")],
      ["Criteria", predicateSummary(gate.criteria)],
    ]),
  },
  {
    title: "Classification",
    description:
      "Label rules inheriting repositories evaluate before their own; a repository rule wins on the same key.",
    path: "/api/v1/classification-defaults",
    read: "admin:classification-defaults:read",
    write: "admin:classification-defaults:write",
    schema: schemas.classificationDefaults,
    blank: {rules: []},
    summary: (config) => definitionList([
      ["Rules", classificationSummary(config.rules)],
    ]),
  },
  {
    title: "Trust policy",
    description:
      "Verification policy a repository inherits until it defines its own; a repository policy overrides it entirely.",
    path: "/api/v1/trust-policy-defaults",
    read: "admin:trust-policy-defaults:read",
    write: "admin:trust-policy-defaults:write",
    schema: schemas.trustPolicyDefaults,
    blank: {
      mode: "audit",
      publicKeys: [],
      certificateAuthorities: [],
      allowedIdentities: [],
      deniedFingerprints: [],
    },
    summary: (policy) => definitionList([
      ["Mode", policy.mode || "—"],
      ["Public keys", String(policy.publicKeys?.length || 0)],
      ["Certificate authorities", String(policy.certificateAuthorities?.length || 0)],
      ["Allowed identities", String(policy.allowedIdentities?.length || 0)],
    ]),
  },
];

export async function renderDefaultsRoute(view, route, identity) {
  if (route.segments[0] !== "defaults" || route.segments.length !== 1) {
    return false;
  }
  view.replaceChildren(heading(
    "Policy defaults",
    "Instance-wide defaults that repositories inherit. A default declared in the deployment's " +
      "provisioning document is managed and read-only here — edit it there. A default set from this " +
      "page stays editable here.",
  ));
  for (const panel of panels) {
    view.append(await renderDefaultPanel(panel, identity));
  }
  return true;
}

async function renderDefaultPanel(panel, identity) {
  const section = element("section", "panel");
  const header = element("div", "panel-heading");
  header.append(element("h2", "", panel.title));
  const body = element("div");
  section.append(header, body);

  if (!hasPrivilege(identity, panel.read)) {
    body.append(element("p", "", `Requires ${panel.read}.`));
    return section;
  }

  let current = null;
  try {
    current = await api.json(panel.path);
  } catch (error) {
    if (error.status !== 404) {
      body.append(element("p", "inline-error", error.message));
      return section;
    }
  }

  body.append(element("p", "", panel.description));
  const canWrite = hasPrivilege(identity, panel.write);
  const managed = Boolean(current?.managed);
  if (managed) {
    header.append(badge("managed"));
  }
  const actions = element("div", "button-row");

  if (!current) {
    body.append(element(
      "p",
      "muted",
      "No instance-wide default is configured; inheriting repositories fall back to their own policy only.",
    ));
    if (canWrite) {
      actions.append(button("Set default", () => editDefault(panel, panel.blank, "create")));
    }
  } else {
    body.append(panel.summary(current));
    if (managed) {
      body.append(element(
        "p",
        "muted",
        "Managed by declarative provisioning; edit it in the provisioning document.",
      ));
    } else if (canWrite) {
      actions.append(button("Edit", () => editDefault(panel, current, "edit")));
      actions.append(button("Delete", () => deleteDefault(panel), "danger"));
    }
  }

  if (actions.childElementCount) {
    body.append(actions);
  }
  return section;
}

function editDefault(panel, value, mode) {
  openForm({
    title: `${mode === "create" ? "Set" : "Edit"} ${panel.title.toLowerCase()} default`,
    description: panel.description,
    schema: panel.schema,
    value: editableDefault(value),
    mode,
    saveLabel: mode === "create" ? "Set default" : "Save changes",
    onSave: async (payload) => {
      await api.json(panel.path, {method: "PUT", body: payload});
      showNotice(`${panel.title} default was saved.`);
      refreshCurrentRoute();
    },
    onDelete: mode === "edit" ? async () => {
      await api.json(panel.path, {method: "DELETE"});
      showNotice(`${panel.title} default was cleared.`);
      refreshCurrentRoute();
    } : null,
    deleteMessage: `Clear the instance-wide ${panel.title.toLowerCase()} default?`,
  });
}

async function deleteDefault(panel) {
  if (!await confirmAction(`Clear the instance-wide ${panel.title.toLowerCase()} default?`)) {
    return;
  }
  await api.json(panel.path, {method: "DELETE"});
  showNotice(`${panel.title} default was cleared.`);
  refreshCurrentRoute();
}

// Strip server-managed and repository-only fields so the form value carries only
// the request body the defaults endpoints accept.
function editableDefault(value) {
  const copy = cloneJSON(value);
  delete copy.managed;
  delete copy.updatedAt;
  delete copy.repository;
  delete copy.inheritGlobal;
  return copy;
}

function classificationSummary(rules) {
  if (!Array.isArray(rules) || rules.length === 0) {
    return "—";
  }
  return rules
    .map((rule) => `${rule.key ?? "?"}=${rule.value ?? "?"}`)
    .join(" · ");
}
