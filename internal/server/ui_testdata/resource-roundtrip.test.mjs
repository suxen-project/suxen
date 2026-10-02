import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import vm from "node:vm";

import {mergeFormValue} from "../ui/form_values.js";
import {schemas} from "../ui/schemas.js";
import {cloneJSON} from "../ui/json_codec.js";

const source = readFileSync(new URL("../ui/resource_views.js", import.meta.url), "utf8");
const projection = source.match(/const editableFields = \{[\s\S]*?\n\};\n\nfunction editableValue\([\s\S]*?\n\}\n\nasync function renderTokens/);
assert.ok(projection, "resource edit projection is available for the round-trip test");
const scope = vm.createContext({structuredClone, cloneJSON});
vm.runInContext(projection[0].replace(/\n\nasync function renderTokens$/, ""), scope);
const editableValue = vm.runInContext("editableValue", scope);

test("generic GET/edit/PUT round trips include only writable fields", () => {
  const readonly = {
    managed: false, createdAt: "2026-09-24T12:00:00Z", updatedAt: "2026-09-24T12:00:00Z",
    writable: true, id: 42, state: "draining", drainTarget: "archive",
  };
  const cases = [
    {resource: "blob-stores", schema: schemas.blobStore,
      response: {name: "archive", driver: "fs", configurationRef: {env: "ARCHIVE"}, attributes: {uploadSessions: {staleAfter: "6h"}}},
      fields: ["driver", "configurationRef", "attributes"]},
    {resource: "cleanup-policies", schema: schemas.cleanupPolicy,
      response: {name: "old", repositories: ["raw"], criteria: [{path: "sys.size", op: ">", value: 10}],
        keepLast: 2, order: "version", action: "delete", enabled: true},
      fields: ["repositories", "criteria", "keepLast", "order", "action", "enabled"]},
    {resource: "users", schema: schemas.user,
      response: {username: "publisher", admin: false, roles: ["writer"]},
      fields: ["password", "admin", "roles"]},
    {resource: "roles", schema: schemas.role,
      response: {name: "writer", description: "publisher", privileges: ["repository:raw:write"]},
      fields: ["description", "privileges"]},
    {resource: "oidc-providers", schema: schemas.oidc,
      response: {name: "corp", issuer: "https://id.example", clientId: "suxen", scopes: ["openid"],
        groupsClaim: "groups", defaultRoles: ["reader"], groupRoles: {ops: ["admin"]}, allowPasswordGrant: true},
      fields: ["issuer", "clientId", "clientSecret", "scopes", "groupsClaim", "defaultRoles",
        "groupRoles", "allowPasswordGrant"]},
    {resource: "webhooks", schema: schemas.webhook,
      response: {name: "scanner", url: "https://scanner.example/hook", events: ["asset.uploaded"],
        repositories: ["raw"], enabled: true},
      fields: ["url", "secret", "events", "repositories", "enabled"]},
  ];
  for (const entry of cases) {
    const response = {...readonly, ...entry.response};
    const editable = editableValue(entry.resource, response, "edit");
    const regularFields = Object.fromEntries(entry.schema
      .filter((field) => !field.createOnly)
      .map((field) => [field.name, editable[field.name]]));
    const request = mergeFormValue(editable, regularFields);
    assert.deepEqual(Object.keys(request).sort(), [...entry.fields].sort(), entry.resource);
    assert.equal(Object.keys(request).some((name) => ["state", "drainTarget", "managed", "createdAt", "updatedAt", "id"].includes(name)), false,
      entry.resource);
    assert.equal(Object.hasOwn(response, "managed"), true, "GET response was not mutated");
    if (entry.resource === "oidc-providers") {
      // A schema can lag an API writable field without a save clearing it.
      const withoutGrantControl = {...regularFields};
      delete withoutGrantControl.allowPasswordGrant;
      assert.equal(mergeFormValue(editable, withoutGrantControl).allowPasswordGrant, true);
    }
    if (["users", "oidc-providers", "webhooks"].includes(entry.resource)) {
      const secret = {users: "password", "oidc-providers": "clientSecret", webhooks: "secret"}[entry.resource];
      assert.equal(request[secret], "", `${entry.resource} must not echo a redacted secret`);
    }
  }
});
