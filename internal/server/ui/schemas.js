"use strict";

const text = (name, label, options = {}) => ({name, label, type: "text", ...options});
const list = (name, label, options = {}) => ({name, label, type: "list", ...options});
const json = (name, label, options = {}) => ({name, label, type: "json", ...options});

export const webhookEvents = [
  "asset.uploaded",
  "asset.deleted",
  "asset.downloaded",
  "component.created",
  "cleanup.completed",
];

export const schemas = {
  repository: [
    text("name", "Name", {required: true, createOnly: true, help: "Lowercase letters, numbers, '-' and '_'."}),
    {name: "format", label: "Format", type: "select", required: true, options: ["raw", "oci", "maven", "git", "go", "cargo", "npm", "pypi"],
      help: "Formats compiled into this binary. The UI loads the live list from GET /api/v1."},
    {name: "type", label: "Type", type: "select", required: true, options: ["hosted", "proxy", "group"]},
    {name: "allowOverwrite", label: "Replace existing assets", type: "select", options: ["Format default", "Allow", "Deny"],
      help: "Hosted repositories only. Identical retries remain allowed. Defaults: allow for Raw, OCI, Maven and Go; deny for npm, Cargo and PyPI."},
    text("blobStore", "Blob store", {placeholder: "default"}),
    text("upstream", "Upstream URL", {help: "Required for proxies. Leave unchanged to retain stored credentials and query parameters; enter a new URL to replace them."}),
    list("members", "Group members", {help: "Repository names, one per row."}),
    json("formatConfig", "Format configuration", {
      help: "Format-specific settings, for example Maven {\"versionPolicy\":\"release\"} or Raw {\"components\":[{\"pattern\":\"^(?P<name>.+)/(?P<version>[^/]+)/[^/]+$\"}]}.",
    }),
    json("endpoints", "OCI endpoints", {
      help: "Optional hosts and extra listen ports so Docker can use this repository at a registry root. Example: {\"hosts\":[\"registry.example.com\"],\"ports\":[5000]}.",
    }),
  ],
  blobStore: [
    text("name", "Name", {required: true, createOnly: true}),
    text("driver", "Driver", {required: true, placeholder: "fs or s3"}),
    {
      name: "configurationRef",
      label: "Configuration reference",
      type: "keyvalue",
      help: "Use env or file, for example env=SUXEN_ARCHIVE_STORE.",
    },
    json("attributes", "Attributes", {
      help: "uploadSessions controls expiry and staged-upload quotas for this store.",
    }),
  ],
  cleanupPolicy: [
    text("name", "Name", {required: true, createOnly: true}),
    list("repositories", "Repositories", {required: true}),
    {
      name: "criteria",
      label: "Selection predicates",
      type: "predicates",
      required: true,
      help: "All predicates must match. sys.blobStore can target an aggressively cleaned store.",
    },
    {name: "keepLast", label: "Keep newest matches", type: "number", min: 0},
    {name: "order", label: "Retention order", type: "select", options: ["updatedAt", "version"],
      help: "updatedAt keeps the most recently updated; version keeps the highest Raw component version or OCI tag."},
    {name: "action", label: "Action", type: "select", options: ["delete"]},
    {name: "enabled", label: "Enabled", type: "boolean",
      help: "Enabled policies run on the shared cleanup interval; there is no per-policy schedule."},
  ],
  user: [
    text("username", "Username", {required: true, createOnly: true}),
    {
      name: "password",
      label: "Password",
      type: "password",
      help: "Required on create; leave empty on edit to preserve.",
    },
    {name: "admin", label: "Administrator", type: "boolean"},
    list("roles", "Roles"),
  ],
  role: [
    text("name", "Name", {required: true, createOnly: true}),
    text("description", "Description"),
    list("privileges", "Privileges", {help: "Example: repository:raw:write"}),
  ],
  oidc: [
    text("name", "Name", {required: true, createOnly: true}),
    text("issuer", "Issuer URL", {required: true, inputMode: "url"}),
    text("clientId", "Client ID", {required: true}),
    {name: "clientSecret", label: "Client secret", type: "password", help: "Leave empty on edit to preserve."},
    list("scopes", "Scopes"),
    text("groupsClaim", "Groups claim", {placeholder: "groups"}),
    list("defaultRoles", "Default roles"),
    {name: "groupRoles", label: "Group to roles", type: "keyvalue", valueType: "list"},
    {name: "allowPasswordGrant", label: "Allow password grant", type: "boolean",
      help: "For trusted internal identity providers that support password grants."},
  ],
  webhook: [
    text("name", "Name", {required: true, createOnly: true}),
    text("url", "Destination URL", {required: true, inputMode: "url"}),
    {name: "secret", label: "HMAC secret", type: "password", help: "At least 16 characters; leave empty on edit."},
    {name: "events", label: "Events", type: "checks", required: true, options: webhookEvents},
    list("repositories", "Repositories", {help: "Empty means all repositories."}),
    {name: "enabled", label: "Enabled", type: "boolean"},
  ],
  classification: [
    json("rules", "Rules", {
      help: "Ordered list of {\"when\":[<predicate>...],\"key\":\"<label key>\",\"value\":\"<label value>\"}. " +
        "Every matching rule sets classification.<key>=<value>; a later rule overwrites the same key; " +
        "an asset matched by no rule gets no classification.* labels. when predicates use the same " +
        "{path, op, value} form as gate criteria, e.g. {\"path\":\"sys.size\",\"op\":\">\",\"value\":1073741824}.",
    }),
    {
      name: "inheritGlobal",
      label: "Inherit instance default",
      type: "boolean",
      help: "When on, the instance-wide default classification rules are evaluated before this repository's " +
        "(a repository rule wins on the same key). Saving re-labels existing assets from the merged rules.",
    },
  ],
  downloadGate: [
    {
      name: "criteria",
      label: "Required predicates",
      type: "predicates",
      help: "Every predicate must match the projected asset attributes before download is allowed.",
    },
    {name: "enabled", label: "Enabled", type: "boolean"},
    {
      name: "inheritGlobal",
      label: "Inherit instance default",
      type: "boolean",
      help: "When on, the instance-wide default gate's predicates are also required (AND). " +
        "Turn off to gate on this repository's predicates alone; leave the predicates empty to gate nothing.",
    },
  ],
  trustPolicy: [
    {name: "mode", label: "Mode", type: "select", options: ["audit", "verify-on-pull", "verify-on-push"]},
    list("publicKeys", "Public keys", {multiline: true}),
    list("certificateAuthorities", "Certificate authorities", {multiline: true}),
    json("allowedIdentities", "Allowed certificate identities"),
    list("deniedFingerprints", "Denied fingerprints"),
  ],
  classificationDefaults: [
    json("rules", "Rules", {
      help: "Instance-wide default label rules, same shape as a repository's: ordered " +
        "{\"when\":[<predicate>...],\"key\":\"<label key>\",\"value\":\"<label value>\"}. Inheriting " +
        "repositories evaluate these before their own; saving re-labels every inheriting repository's assets.",
    }),
  ],
  downloadGateDefaults: [
    {
      name: "criteria",
      label: "Required predicates",
      type: "predicates",
      help: "Inheriting repositories AND these predicates onto their own before allowing a download.",
    },
    {name: "enabled", label: "Enabled", type: "boolean"},
  ],
  trustPolicyDefaults: [
    {name: "mode", label: "Mode", type: "select", options: ["audit", "verify-on-pull", "verify-on-push"]},
    list("publicKeys", "Public keys", {multiline: true}),
    list("certificateAuthorities", "Certificate authorities", {multiline: true}),
    json("allowedIdentities", "Allowed certificate identities"),
    list("deniedFingerprints", "Denied fingerprints"),
  ],
  attributes: [
    text("namespace", "Namespace", {required: true}),
    json("value", "Value", {required: true}),
  ],
  verification: [
    text("signature", "Signature", {multiline: true}),
    text("payload", "Payload", {multiline: true}),
    text("certificate", "Certificate", {multiline: true}),
    list("certificateChain", "Certificate chain", {multiline: true}),
  ],
  token: [
    text("name", "Token name", {required: true}),
    list("scopes", "Scopes", {help: "Empty uses role privileges."}),
  ],
};

export const defaults = {
  repository: {name: "releases", format: "raw", type: "hosted", blobStore: "default", upstream: "", members: [], formatConfig: {}, endpoints: {}},
  blobStore: {
    name: "archive",
    driver: "s3",
    configurationRef: {env: "SUXEN_ARCHIVE_STORE"},
    attributes: {
      uploadSessions: {
        staleAfter: "6h",
        maxStagedBytes: 5368709120,
        maxPrincipalStagedBytes: 1073741824,
        maxPrincipalSessions: 4,
      },
    },
  },
  cleanupPolicy: {
    name: "discard-prereleases",
    repositories: ["raw"],
    criteria: [{path: "sys.lastAccessed", op: "before", value: "30d"}],
    keepLast: 5,
    action: "delete",
    enabled: false,
  },
  user: {username: "publisher", password: "", admin: false, roles: []},
  role: {name: "publisher", description: "Artifact publisher", privileges: ["repository:*:read"]},
  oidc: {
    name: "corporate",
    issuer: "https://identity.example/realms/engineering",
    clientId: "suxen",
    clientSecret: "",
    scopes: ["openid", "profile", "email", "groups"],
    groupsClaim: "groups",
    defaultRoles: [],
    groupRoles: {},
  },
  webhook: {
    name: "scanner",
    url: "https://scanner.example/hooks/suxen",
    secret: "",
    events: ["asset.uploaded"],
    repositories: [],
    enabled: true,
  },
};
