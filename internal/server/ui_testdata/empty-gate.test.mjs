import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import vm from "node:vm";
import {schemas} from "../ui/schemas.js";
import {mergeFormValue} from "../ui/form_values.js";

// Submit the shared form with its real schema validation and empty predicate
// input, preserving cleanup's stricter contract while allowing gate opt-outs.
function formHarness(schema, value) {
  const nodes = new Map();
  const document = {querySelector(selector) {
    if (!nodes.has(selector)) nodes.set(selector, {
      listeners: {},
      addEventListener(event, callback) { this.listeners[event] = callback; },
    });
    return nodes.get(selector);
  }};
  const saved = [];
  const scope = vm.createContext({document, mergeFormValue,
    config: {value, onSave: async (payload) => saved.push(payload)},
    readers: schema.map((descriptor) => ({descriptor, error: {}, read: () => value[descriptor.name]})),
  });
  const source = readFileSync(new URL("../ui/components.js", import.meta.url), "utf8")
    .replace(/import\s[\s\S]*?from\s+"[^"]+";/g, "")
    .replace(/export /g, "");
  vm.runInContext(source, scope);
  vm.runInContext("formConfiguration = config; fieldReaders = readers;", scope);
  return {saved, nodes, submit: () => nodes.get("#resource-form").listeners.submit({preventDefault() {}})};
}

test("forms allow explicit empty repository and instance gates but reject empty cleanup", async () => {
  for (const schema of [schemas.downloadGate, schemas.downloadGateDefaults]) {
    const value = {criteria: [], enabled: true, inheritGlobal: false};
    const form = formHarness(schema, value);
    await form.submit();
    assert.equal(form.saved.length, 1, "empty gate should be sent as an explicit opt-out");
    assert.deepEqual(form.saved[0].criteria, []);
    assert.equal(form.saved[0].inheritGlobal, false);
  }
  const form = formHarness(schemas.cleanupPolicy, {
    name: "expired", repositories: ["raw"], criteria: [], keepLast: 0, action: "delete", enabled: true,
  });
  await form.submit();
  assert.equal(form.saved.length, 0, "empty cleanup must not select all artifacts");
  assert.equal(form.nodes.get("#form-error").hidden, false);
});
