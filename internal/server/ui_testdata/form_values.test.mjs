import assert from "node:assert/strict";
import test from "node:test";

import {mergeFormValue} from "../ui/form_values.js";

test("regular and Advanced JSON form values retain editable fields absent from the schema", () => {
  const original = {
    format: "maven", formatConfig: {versionPolicy: "release"},
    allowPasswordGrant: true, extensionSetting: {limit: 4},
  };
  const current = mergeFormValue(original, {format: "maven", formatConfig: {versionPolicy: "release"}});
  assert.equal(current.allowPasswordGrant, true);
  assert.deepEqual(current.extensionSetting, {limit: 4});
  assert.deepEqual(JSON.parse(JSON.stringify(current)), current);
  current.extensionSetting.limit = 8;
  assert.equal(original.extensionSetting.limit, 4);
});
