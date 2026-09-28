import assert from "node:assert/strict";
import test from "node:test";

import {requestDialogDecision} from "../ui/dialog.js";

class FakeDialog extends EventTarget {
  returnValue = "";

  open = false;

  showModal() {
    this.open = true;
  }

  close(returnValue) {
    if (returnValue !== undefined) {
      this.returnValue = returnValue;
    }
    this.open = false;
    this.dispatchEvent(new Event("close"));
  }
}

test("a previous confirmation cannot leak into a later cancellation", async () => {
  const dialog = new FakeDialog();
  const confirmed = requestDialogDecision(dialog);
  dialog.close("confirm");
  assert.equal(await confirmed, true);

  let destructiveCalls = 0;
  const cancelled = requestDialogDecision(dialog);
  assert.equal(dialog.returnValue, "");
  dialog.close();
  if (await cancelled) {
    destructiveCalls += 1;
  }

  assert.equal(destructiveCalls, 0);
});
