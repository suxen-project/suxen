import assert from "node:assert/strict";
import test from "node:test";

import {defaults, schemas, webhookEvents} from "../ui/schemas.js";

test("webhook forms expose only the stable event vocabulary", () => {
  assert.deepEqual(webhookEvents, [
    "asset.uploaded",
    "asset.deleted",
    "asset.downloaded",
    "component.created",
    "cleanup.completed",
  ]);
  const eventField = schemas.webhook.find((field) => field.name === "events");
  assert.equal(eventField.type, "checks");
  assert.deepEqual(eventField.options, webhookEvents);
  assert.equal(schemas.webhook.find((field) => field.name === "name").createOnly, true);
});

test("false boolean defaults and typed cleanup predicates are preserved", () => {
  assert.equal(defaults.cleanupPolicy.enabled, false);
  assert.equal(defaults.user.admin, false);
  assert.equal(schemas.cleanupPolicy.find((field) => field.name === "criteria").type, "predicates");
  assert.equal(defaults.cleanupPolicy.criteria[0].path, "sys.lastAccessed");
});

test("repository forms expose OCI endpoint bindings", () => {
  const endpoints = schemas.repository.find((field) => field.name === "endpoints");
  assert.equal(endpoints.type, "json");
  assert.deepEqual(defaults.repository.endpoints, {});
  assert.equal(schemas.repository.find((field) => field.name === "formatConfig").type, "json");
  assert.deepEqual(defaults.repository.formatConfig, {});
});

test("OIDC forms expose password grant configuration", () => {
  assert.equal(schemas.oidc.find((field) => field.name === "allowPasswordGrant").type, "boolean");
});

test("blob store forms expose upload-session policy attributes", () => {
  const attributes = schemas.blobStore.find((field) => field.name === "attributes");
  assert.equal(attributes.type, "json");
  assert.equal(defaults.blobStore.attributes.uploadSessions.staleAfter, "6h");
  assert.equal(defaults.blobStore.attributes.uploadSessions.maxPrincipalSessions, 4);
});
