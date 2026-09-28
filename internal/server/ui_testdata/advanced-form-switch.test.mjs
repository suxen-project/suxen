import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import vm from "node:vm";

import {mergeFormValue} from "../ui/form_values.js";
import {cloneJSON, isExactJSONNumber, parseJSON, stringifyJSON} from "../ui/json_codec.js";

function formHarness() {
  const nodes = new Map();
  const document = {querySelector(selector) {
    if (!nodes.has(selector)) {
      nodes.set(selector, {
        listeners: {},
        addEventListener(event, callback) { this.listeners[event] = callback; },
        querySelector() { return null; },
        focus() { this.focused = true; },
        showModal() { this.open = true; },
        close() { this.open = false; },
      });
    }
    return nodes.get(selector);
  }};
  const source = readFileSync(new URL("../ui/components.js", import.meta.url), "utf8")
    .replace(/import\s[\s\S]*?from\s+"[^"]+";/g, "")
    .replace(/export /g, "");
  const scope = vm.createContext({document, mergeFormValue, cloneJSON, isExactJSONNumber, parseJSON, stringifyJSON});
  vm.runInContext(source, scope);
  // Keep the real mode-switch and submit handlers; only the DOM control factory
  // is replaced so this test can drive the form without a browser dependency.
  vm.runInContext(`
    let normalName;
    buildFields = (_schema, value) => {
      normalName = value.name;
      fieldReaders = [{descriptor: {name: "name", label: "Name"},
        error: {textContent: ""}, read: () => normalName}];
    };
  `, scope);
  return {
    nodes,
    open: (configuration) => {
      scope.config = configuration;
      vm.runInContext("openForm(config)", scope);
    },
    normalName: () => vm.runInContext("normalName", scope),
    setNormalName: (value) => { scope.normalNameValue = value; vm.runInContext("normalName = normalNameValue", scope); },
    toggle: (open) => {
      nodes.get("#advanced-editor").open = open;
      nodes.get("#advanced-editor").listeners.toggle();
    },
    submit: () => nodes.get("#resource-form").listeners.submit({preventDefault() {}}),
  };
}

test("Advanced JSON edits survive closing, reopening, and saving the regular form", async () => {
  const form = formHarness();
  const saved = [];
  const original = {name: "before", extensionSetting: {limit: 4}};
  form.open({title: "Edit", schema: [], value: original, onSave: async (value) => saved.push(value)});
  form.toggle(true);
  const editor = form.nodes.get("#advanced-json");
  editor.value = JSON.stringify({name: "after", extensionSetting: {limit: 8}, extra: true});
  form.nodes.get("#resource-fields").listeners.input?.();
  assert.equal(JSON.parse(editor.value).name, "after", "normal controls cannot overwrite Advanced JSON");
  form.toggle(false);
  assert.equal(form.normalName(), "after");
  assert.equal(form.nodes.get("#resource-fields").inert, false);
  form.toggle(true);
  assert.deepEqual(JSON.parse(editor.value), {name: "after", extensionSetting: {limit: 8}, extra: true});
  form.toggle(false);
  await form.submit();
  assert.deepEqual(saved, [{name: "after", extensionSetting: {limit: 8}, extra: true}]);
  assert.deepEqual(original, {name: "before", extensionSetting: {limit: 4}});
});

test("invalid Advanced JSON cannot be hidden and saved as stale normal fields", async () => {
  const form = formHarness();
  const saved = [];
  form.open({title: "Edit", schema: [], value: {name: "before"}, onSave: async (value) => saved.push(value)});
  form.toggle(true);
  const editor = form.nodes.get("#advanced-json");
  editor.value = '{"name":';
  form.toggle(false);
  assert.equal(form.nodes.get("#advanced-editor").open, true);
  assert.equal(editor.value, '{"name":');
  assert.equal(form.nodes.get("#resource-fields").inert, true);
  assert.equal(form.nodes.get("#form-error").hidden, false);
  // A queued toggle event after reopening must not copy stale normal controls.
  form.nodes.get("#advanced-editor").listeners.toggle();
  assert.equal(editor.value, '{"name":');
  await form.submit();
  assert.equal(saved.length, 0);
});

test("submit after native collapse but before toggle uses valid Advanced JSON", async () => {
  const form = formHarness();
  const saved = [];
  form.open({title: "Edit", schema: [], value: {name: "before"}, onSave: async (value) => saved.push(value)});
  form.toggle(true);
  form.nodes.get("#advanced-json").value = '{"name":"after","extra":true}';
  form.nodes.get("#advanced-editor").open = false; // toggle event is queued by the browser
  await form.submit();
  assert.deepEqual(JSON.parse(JSON.stringify(saved)), [{name: "after", extra: true}]);
});

test("submit after native collapse but before toggle rejects invalid Advanced JSON", async () => {
  const form = formHarness();
  const saved = [];
  form.open({title: "Edit", schema: [], value: {name: "before"}, onSave: async (value) => saved.push(value)});
  form.toggle(true);
  form.nodes.get("#advanced-json").value = '{"name":';
  form.nodes.get("#advanced-editor").open = false; // toggle event is queued by the browser
  await form.submit();
  assert.equal(saved.length, 0);
  assert.equal(form.nodes.get("#form-error").hidden, false);
});

test("Advanced JSON requires an object even for exact numeric literals", async () => {
  const form = formHarness();
  const saved = [];
  form.open({title: "Edit", schema: [], value: {name: "before"}, onSave: async (value) => saved.push(value)});
  form.toggle(true);
  form.nodes.get("#advanced-json").value = "1e1000";
  await form.submit();
  assert.equal(saved.length, 0);
  assert.match(form.nodes.get("#form-error").textContent, /must be an object/);
});

test("submit after native open but before toggle uses current normal fields", async () => {
  const form = formHarness();
  const saved = [];
  form.open({title: "Edit", schema: [], value: {name: "before"}, onSave: async (value) => saved.push(value)});
  form.setNormalName("after");
  form.nodes.get("#advanced-editor").open = true; // toggle event has not copied the normal fields yet
  await form.submit();
  assert.deepEqual(saved, [{name: "after"}]);
});
