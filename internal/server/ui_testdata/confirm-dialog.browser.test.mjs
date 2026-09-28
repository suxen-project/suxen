import assert from "node:assert/strict";
import {spawnSync} from "node:child_process";
import {existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {fileURLToPath, pathToFileURL} from "node:url";
import test from "node:test";

const browserCandidates = [
  process.env.CHROMIUM,
  "/usr/bin/chromium",
  "/usr/bin/chromium-browser",
  "/usr/bin/google-chrome",
].filter(Boolean);
const browser = browserCandidates.find(existsSync);

test("native cancel cannot reuse an earlier confirmation", {skip: !browser}, (context) => {
  const profile = mkdtempSync(join(tmpdir(), "suxen-ui-browser-"));
  const implementationPath = fileURLToPath(new URL("../ui/dialog.js", import.meta.url));
  const implementation = readFileSync(implementationPath, "utf8").replace("export function", "function");
  const fixture = join(profile, "confirm-dialog.html");
  writeFileSync(fixture, `<!doctype html>
    <body data-result="pending">
      <dialog id="confirm-dialog">
        <form method="dialog">
          <button id="cancel" value="cancel">Cancel</button>
          <button id="confirm" value="confirm">Confirm</button>
        </form>
      </dialog>
      <script>
        ${implementation}
        (async () => {
          const dialog = document.querySelector("#confirm-dialog");
          const firstDecision = requestDialogDecision(dialog);
          dialog.close("confirm");
          dialog.dispatchEvent(new Event("close"));
          const firstConfirmed = await firstDecision;
          let destructiveCalls = 0;
          const secondDecision = requestDialogDecision(dialog);
          document.querySelector("#cancel").click();
          dialog.dispatchEvent(new Event("close"));
          if (await secondDecision) {
            destructiveCalls += 1;
          }
          document.body.dataset.result = firstConfirmed && destructiveCalls === 0
            ? "passed"
            : "failed";
        })().catch((error) => {
          document.body.dataset.result = "failed";
          document.body.dataset.error = error.message;
        });
      </script>
    </body>`, "utf8");
  try {
    const result = spawnSync(browser, [
      "--headless=new",
      "--no-sandbox",
      "--disable-gpu",
      "--disable-dev-shm-usage",
      "--allow-file-access-from-files",
      "--virtual-time-budget=2000",
      `--user-data-dir=${profile}`,
      "--dump-dom",
      pathToFileURL(fixture).href,
    ], {encoding: "utf8", timeout: 15_000, killSignal: "SIGKILL"});
    if (result.status === null && result.stderr.includes("Operation not permitted")) {
      context.skip("Chromium cannot start in this process sandbox.");
      return;
    }
    // Chromium can render --dump-dom and then keep a background process alive.
    // Require the observed browser result, while bounding the process lifetime.
    if (result.error && result.error.code !== "ETIMEDOUT") throw result.error;
    if (result.error?.code !== "ETIMEDOUT") assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /data-result="passed"/, result.stderr);
  } finally {
    rmSync(profile, {recursive: true, force: true});
  }
});
