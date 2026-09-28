import assert from "node:assert/strict";
import {spawn} from "node:child_process";
import {existsSync, mkdtempSync, readFileSync, rmSync} from "node:fs";
import {createServer} from "node:http";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {fileURLToPath} from "node:url";
import test from "node:test";

const browserCandidates = [
  process.env.CHROMIUM,
  "/usr/bin/chromium",
  "/usr/bin/chromium-browser",
  "/usr/bin/google-chrome",
].filter(Boolean);
const browser = browserCandidates.find(existsSync);

test("native cancel cannot reuse an earlier confirmation", {skip: !browser}, async () => {
  const profile = mkdtempSync(join(tmpdir(), "suxen-ui-browser-"));
  const implementationPath = fileURLToPath(new URL("../ui/dialog.js", import.meta.url));
  const implementation = readFileSync(implementationPath, "utf8").replace("export function", "function");
  const fixture = `<!doctype html>
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
          const status = firstConfirmed && destructiveCalls === 0 ? "passed" : "failed";
          fetch("/result?status=" + status);
        })().catch((error) => {
          fetch("/result?status=" + encodeURIComponent(error.message));
        });
      </script>
    </body>`;

  let resolveResult;
  const result = new Promise((resolve) => { resolveResult = resolve; });
  const server = createServer((request, response) => {
    const url = new URL(request.url, "http://127.0.0.1");
    if (url.pathname === "/result") {
      resolveResult(url.searchParams.get("status"));
      response.writeHead(204).end();
      return;
    }
    response.writeHead(200, {"Content-Type": "text/html; charset=utf-8"}).end(fixture);
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  const child = spawn(browser, [
    "--headless=new",
    "--no-sandbox",
    "--disable-gpu",
    "--disable-dev-shm-usage",
    "--remote-debugging-port=0",
    `--user-data-dir=${profile}`,
    `http://127.0.0.1:${address.port}/`,
  ], {stdio: ["ignore", "ignore", "pipe"]});
  let stderr = "";
  child.stderr.on("data", (chunk) => { stderr = (stderr + chunk).slice(-8192); });
  const exit = new Promise((_, reject) => {
    child.once("error", reject);
    child.once("exit", (code, signal) => reject(new Error(
      `browser exited before reporting: ${code ?? signal}\n${stderr}`,
    )));
  });
  let deadline;
  try {
    const timeout = new Promise((_, reject) => {
      deadline = setTimeout(() => reject(new Error(`browser timed out:\n${stderr}`)), 25_000);
    });
    assert.equal(await Promise.race([result, exit, timeout]), "passed");
  } finally {
    clearTimeout(deadline);
    child.kill("SIGKILL");
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    rmSync(profile, {recursive: true, force: true});
  }
});
