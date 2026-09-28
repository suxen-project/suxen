import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import vm from "node:vm";
import {cloneJSON, parseJSON, stringifyJSON} from "../ui/json_codec.js";

function predicateEditor(value) {
  const nodes = [];
  const document = {
    createElement(tag) {
      const node = {tag, children: [], listeners: {}, value: "",
        append(...children) { this.children.push(...children); },
        replaceChildren(...children) { this.children = children; },
        setAttribute() {},
        addEventListener(event, callback) { this.listeners[event] = callback; },
      };
      nodes.push(node);
      return node;
    },
    querySelector() { return this.createElement("div"); },
  };
  const scope = {document, cloneJSON, parseJSON, stringifyJSON,
    api: {json: async () => ({attributePaths: []})}};
  const source = readFileSync(new URL("../ui/components.js", import.meta.url), "utf8")
    .replace(/import\s[\s\S]*?from\s+"[^"]+";/g, "")
    .replace(/export /g, "");
  vm.runInNewContext(source, scope);
  const control = scope.predicatesControl([{path: "scan.result", op: "=", value}]);
  const inputs = nodes.filter((node) => node.tag === "input");
  return {control, path: inputs[0], literal: inputs[1]};
}

test("editing a predicate preserves the saved literal's type and whitespace", () => {
  for (const value of ["true", "false", "null", "42", "1e3", "\"quoted\"", "[1]", "{}",
    " padded ", "", true, false, null, 42, ["true", 2]]) {
    const {control, path} = predicateEditor(value);
    assert.deepEqual(structuredClone(control.read()[0].value), value, `opened ${JSON.stringify(value)}`);
    path.value = "scan.other";
    path.listeners.input();
    assert.deepEqual(structuredClone(control.read()[0].value), value, `changed path for ${JSON.stringify(value)}`);
  }
});

test("predicate literal edits still accept JSON values and plain text", () => {
  const {control, literal} = predicateEditor("initial");
  for (const [input, value] of [["false", false], ['"false"', "false"], ["passed", "passed"],
    ['["a", "b"]', ["a", "b"]]]) {
    literal.value = input;
    literal.listeners.input();
    assert.equal(JSON.stringify(control.read()[0].value), JSON.stringify(value));
  }
});

test("predicate editor retains exact numeric literals and nested values", () => {
  const original = parseJSON('[9007199254740993,0.12345678901234567890123456789,1e1000]');
  const {control, path, literal} = predicateEditor(original);
  assert.equal(literal.value, '[9007199254740993,0.12345678901234567890123456789,1e1000]');
  path.value = "scan.other";
  path.listeners.input();
  assert.equal(stringifyJSON(control.read()[0].value), literal.value);
  literal.value = '[1e1001,"1e1000",{"nested":9007199254740995}]';
  literal.listeners.input();
  assert.equal(stringifyJSON(control.read()[0].value), literal.value);
});
