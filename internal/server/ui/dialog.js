"use strict";

// requestDialogDecision opens a native dialog with a clean return value and resolves
// true only when this specific invocation closes with the confirmation value.
export function requestDialogDecision(dialog, confirmationValue = "confirm") {
  dialog.returnValue = "";
  dialog.showModal();
  return new Promise((resolve) => {
    dialog.addEventListener("close", () => {
      resolve(dialog.returnValue === confirmationValue);
    }, {once: true});
  });
}
