import assert from "node:assert/strict";
import test from "node:test";

import {cloneJSON, isExactJSONNumber, parseJSON, stringifyJSON} from "../ui/json_codec.js";
import {mergeFormValue} from "../ui/form_values.js";

globalThis.sessionStorage = {getItem: () => "", setItem() {}, removeItem() {}};

test("JSON codec preserves numeric tokens through nested arrays, maps and clones", () => {
  const source = '{"criteria":[{"value":[9007199254740993,0.12345678901234567890123456789,1e1000,-0,' +
    '{"nested":1.2300}]}],"safe":42,"text":"9007199254740993","__proto__":{"x":1e1000}}';
  const parsed = parseJSON(source);
  assert.equal(parsed.safe, 42);
  assert.equal(typeof parsed.safe, "number");
  assert.equal(parsed.text, "9007199254740993");
  assert.equal(Object.hasOwn(parsed, "__proto__"), true);
  assert.equal(isExactJSONNumber(parsed.criteria[0].value[0]), true);
  assert.equal(isExactJSONNumber(parsed.text), false);
  assert.equal(parseJSON("1e3") > 999, true);
  assert.equal(parseJSON("1e3") + 1, 1001);
  assert.equal(String(parseJSON("1e1000")), "1e1000");
  assert.equal(stringifyJSON(cloneJSON(parsed)), source);
  assert.equal(stringifyJSON(parsed, true).includes("0.12345678901234567890123456789"), true);
});

test("ordinary objects and strings cannot spoof exact number serialization", () => {
  const spoof = {toString: "1e1000", valueOf: "9007199254740993"};
  assert.equal(stringifyJSON({value: spoof, text: "1e1000"}),
    '{"value":{"toString":"1e1000","valueOf":"9007199254740993"},"text":"1e1000"}');
  assert.equal(isExactJSONNumber(spoof), false);
  assert.throws(() => stringifyJSON({value: Infinity}), /Non-finite/);
  assert.throws(() => stringifyJSON({self: {deep: NaN}}), /Non-finite/);
  const cycle = {}; cycle.self = cycle;
  assert.throws(() => stringifyJSON(cycle), /Circular/);
  const shared = {value: parseJSON("1e1000")};
  assert.equal(stringifyJSON({a: shared, b: shared}), '{"a":{"value":1e1000},"b":{"value":1e1000}}');
});

test("parser matches native JSON syntax at number, string and container boundaries", () => {
  for (const source of ['01', '-01', '1.', '1e', '[1,]', '{"x":1,}', '{"x" 1}',
    '"bad\nstring"', '"bad\\x20escape"', '"bad\\u00gg"', 'true false', '[1\v]']) {
    assert.throws(() => JSON.parse(source), SyntaxError, source);
    assert.throws(() => parseJSON(source), SyntaxError, source);
  }
  for (const source of ['{"number":"1e1000","escaped":"\\u0031"}',
    '{"__proto__":1,"constructor":2}', '[true,false,null,"\\uD83D\\uDE00"]']) {
    assert.deepEqual(parseJSON(source), JSON.parse(source));
  }
});

test("real API client and form merge retain exact predicates during unrelated edits", async () => {
  const {api} = await import("../ui/api.js");
  const source = '{"criteria":[{"path":"scan.serial","op":"=","value":9007199254740993},' +
    '{"path":"scan.decimal","op":"in","value":[0.12345678901234567890123456789,1e1000,' +
    '{"deep":-0}]}],"enabled":true,"inheritGlobal":true,"label":"1e1000"}';
  let sent;
  globalThis.fetch = async (_path, options) => {
    if (options.method === "PUT") {
      sent = options.body;
      return new Response(null, {status: 204});
    }
    return new Response(source, {headers: {"Content-Type": "application/json"}});
  };
  const current = await api.json("/api/v1/repositories/raw/download-gate");
  const edited = mergeFormValue(current, {enabled: false});
  await api.json("/api/v1/repositories/raw/download-gate", {method: "PUT", body: edited});
  assert.equal(sent, source.replace('"enabled":true', '"enabled":false'));
  assert.equal(current.enabled, true);
  assert.equal(edited.label, "1e1000");
});
