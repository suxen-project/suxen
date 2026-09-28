"use strict";

import {api, apiPath, queryString} from "./api.js";
import {
  badge,
  button,
  code,
  confirmAction,
  definitionList,
  element,
  formatBytes,
  formatDate,
  heading,
  numberedPager,
  pager,
  prettyJSON,
  renderTable,
  routeLink,
  showNotice,
} from "./components.js";
import {hasPrivilege, routePath} from "./router.js";
import {operationsRouteKind} from "./route_shapes.js";

export async function renderOperationsRoute(view, route, identity) {
  const identifier = route.segments[1];
  const routeKind = operationsRouteKind(route.segments);
  if (routeKind === "overview") {
    await renderOverview(view, identity);
    return true;
  }
  if (!routeKind) {
    return false;
  }
  if (routeKind === "task-detail") {
    await renderTaskDetail(view, identifier);
  } else {
    await renderTasks(view, identity);
  }
  return true;
}

async function renderOverview(view, identity) {
  view.replaceChildren(heading(
    "Repository operations",
    "Storage, delivery, and scheduler status for this Suxen instance.",
    operationalLinks(),
  ));
  const statsGrid = element("div", "stat-grid");
  const recentPanel = panel("Recent tasks");
  view.append(statsGrid, recentPanel.container);

  const requests = [];
  if (hasPrivilege(identity, "admin:stats:read")) {
    requests.push(api.json("/api/v1/stats").then((stats) => renderStats(statsGrid, stats)));
  } else {
    statsGrid.append(element("div", "empty-state", "Storage statistics require admin:stats:read."));
  }
  if (hasPrivilege(identity, "admin:tasks:read")) {
    requests.push(api.page("/api/v1/tasks", {limit: 8}).then((page) => {
      renderTaskTable(recentPanel.body, page.items);
    }));
  } else {
    recentPanel.body.append(element("div", "empty-state", "Task history is not available to this identity."));
  }
  await Promise.all(requests);
}

function operationalLinks() {
  return [
    externalLink("Health", "/healthz"),
    externalLink("Readiness", "/readyz"),
    externalLink("Version", "/version"),
    externalLink("Metrics", "/metrics"),
    externalLink("OpenAPI", "/api/openapi.json"),
  ];
}

function externalLink(label, href) {
  const link = element("a", "button secondary", label);
  link.href = href;
  link.target = "_blank";
  link.rel = "noopener";
  return link;
}

function panel(title, description = "") {
  const container = element("section", "panel");
  const header = element("div", "panel-heading");
  const copy = element("div");
  copy.append(element("h2", "", title));
  if (description) {
    copy.append(element("p", "", description));
  }
  const body = element("div");
  header.append(copy);
  container.append(header, body);
  return {container, header, body};
}

function renderStats(container, stats) {
  const values = [
    ["Repositories", stats.repositories],
    ["Assets", stats.assets],
    ["Unique blobs", stats.uniqueBlobs],
    ["Stored bytes", formatBytes(stats.bytes)],
    ["Webhook queue", stats.webhookQueue],
    ["Dead deliveries", stats.webhookDead],
  ];
  container.replaceChildren();
  for (const [label, value] of values) {
    const card = element("article", "stat-card");
    card.append(element("span", "", label), element("strong", "", value));
    container.append(card);
  }
}

async function renderScheduler(container) {
  const status = await api.json("/api/v1/tasks/leader");
  const intervals = status.intervals || {};
  const rows = [
    ["Cleanup interval", formatInterval(intervals.cleanup)],
    ["Garbage-collection interval", formatInterval(intervals.gc)],
    ["Verify interval", formatInterval(intervals.verify)],
    ["Migrate interval", formatInterval(intervals.migrate)],
  ];
  if (status.lease) {
    rows.push(
      ["Cleanup lease holder", code(status.lease.holder)],
      ["Lease expires", formatDate(status.lease.expiresAt)],
    );
  } else {
    rows.push(["Cleanup lease", "No node currently holds the lease."]);
  }
  container.replaceChildren(definitionList(rows));
}

// formatInterval renders a Go duration string; "0s" (or empty) means the job is
// disabled rather than running every zero seconds.
function formatInterval(value) {
  if (!value || value === "0s") {
    return "disabled";
  }
  return value;
}

async function renderTasks(view, identity) {
  const canRunGC = hasPrivilege(identity, "admin:gc:run");
  view.replaceChildren(heading(
    "Tasks and garbage collection",
    "Trigger cleanup and garbage collection on demand; the table below is the immutable run history.",
  ));

  if (hasPrivilege(identity, "admin:tasks:read")) {
    const schedulerPanel = panel("Scheduler", "Configured job intervals and the current cleanup lease.");
    view.append(schedulerPanel.container);
    try {
      await renderScheduler(schedulerPanel.body);
    } catch (error) {
      schedulerPanel.body.append(element("div", "empty-state", `Could not load scheduler status: ${error.message}`));
    }
  }
  if (canRunGC) {
    view.append(garbageCollectionPanel());
  }
  if (hasPrivilege(identity, "admin:cleanup-policies:read")) {
    try {
      view.append(await cleanupTriggersPanel(identity));
    } catch (error) {
      showNotice(`Could not load cleanup policies: ${error.message}`, true);
    }
  }
  const taskPanel = panel("Task history", "Newest tasks appear first.");
  const controls = element("div");
  taskPanel.container.append(controls);
  view.append(taskPanel.container);
  let rows = [];
  let cursor = "";
  const load = async () => {
    const page = await api.page("/api/v1/tasks", {limit: 50, cursor});
    rows.push(...page.items);
    cursor = page.nextCursor || "";
    renderTaskTable(taskPanel.body, rows);
    controls.replaceChildren(pager(load, Boolean(cursor)));
  };
  await load();
}

function renderTaskTable(container, tasks) {
  renderTable(container, [
    {label: "ID", render: (task) => code(task.id)},
    {label: "Type", key: "type"},
    {label: "Repository", key: "repository"},
    {label: "Policy", key: "policy"},
    {label: "Mode", render: (task) => task.dryRun ? "dry run" : "applied"},
    {label: "Status", render: (task) => badge(task.status)},
    {label: "Created", render: (task) => formatDate(task.createdAt)},
  ], tasks, {
    empty: "No administrative tasks have run yet.",
    href: (task) => routePath("tasks", task.id),
  });
}

async function renderTaskDetail(view, identifier) {
  const task = await api.json(apiPath("api", "v1", "tasks", identifier));
  view.replaceChildren(heading(
    `Task #${task.id}`,
    "Task history is immutable operational evidence.",
    [routeLink("All tasks", routePath("tasks"), "button secondary")],
  ));
  const summary = element("section", "panel detail-panel");
  summary.append(definitionList([
    ["Type", task.type],
    ["Status", badge(task.status)],
    ["Mode", task.dryRun ? "dry run" : "applied"],
    ["Repository", task.repository],
    ["Policy", task.policy],
    ["Created", formatDate(task.createdAt)],
    ["Started", formatDate(task.startedAt)],
    ["Completed", formatDate(task.completedAt)],
    ["Error", task.error],
  ]));
  const result = element("section", "panel");
  result.append(element("h2", "", "Result"));
  result.append(element("pre", "json-preview", prettyJSON(task.result || {})));
  view.append(summary, result);
}

// Lists cleanup policies with on-demand Preview/Run triggers. Enabled policies
// are also run by the backend scheduler; disabled ones run only from here.
async function cleanupTriggersPanel(identity) {
  const trigger = panel(
    "Cleanup policies",
    "Run a policy on demand across its repositories. Preview is a dry run; Run deletes matching assets. " +
      "Enabled policies also run automatically on the shared cleanup interval.",
  );
  const canRun = hasPrivilege(identity, "admin:cleanup-policies:write");
  const table = element("div");
  const controls = element("div");
  const columns = [
    {label: "Name", render: (policy) => code(policy.name)},
    {label: "Repositories", render: (policy) => policy.repositories?.join(", ") || "—"},
    {label: "Scheduling", render: (policy) => badge(policy.enabled ? "scheduled" : "manual only")},
    {
      label: "",
      render: (policy) => {
        if (!canRun) {
          return "—";
        }
        const bar = element("div", "button-row");
        bar.append(button("Preview", () => runCleanupPolicy(policy.name, true), "secondary small"));
        bar.append(button("Run", () => runCleanupPolicy(policy.name, false), "danger small"));
        return bar;
      },
    },
  ];
  const pageSize = 50;
  const load = async (requestedPage) => {
    const page = await api.page("/api/v1/cleanup-policies", {limit: pageSize, page: requestedPage});
    renderTable(table, columns, page.items, {empty: "No cleanup policies are configured."});
    controls.replaceChildren(numberedPager(requestedPage, page.total || 0, pageSize, load));
  };
  trigger.body.append(table, controls);
  await load(1);
  return trigger.container;
}

async function runCleanupPolicy(policyName, dryRun) {
  if (!dryRun && !await confirmAction(
    `Run cleanup policy ${policyName} across its repositories now? Matching assets will be deleted.`,
    {title: "Run cleanup policy", label: "Run cleanup"},
  )) {
    return;
  }
  const query = queryString({dryRun});
  const result = await api.json(
    `${apiPath("api", "v1", "cleanup-policies", policyName)}/run?${query}`,
    {method: "POST"},
  );
  const tasks = result.tasks || [];
  const matched = tasks.reduce((total, task) => total + (task.result?.matched || 0), 0);
  showNotice(
    `${dryRun ? "Preview" : "Cleanup"} of ${policyName}: ${tasks.length} task(s), ${matched} matching asset(s).`,
  );
  // Refresh so the new tasks appear in the history table.
  window.dispatchEvent(new CustomEvent("suxen:resource-changed"));
}

function garbageCollectionPanel() {
  const gcPanel = panel(
    "Blob garbage collection",
    "Preview unreferenced blobs first. Apply uses the same grace period after explicit confirmation.",
  );
  const form = element("form", "inline-form");
  const label = element("label", "", "Grace period");
  const grace = element("input");
  grace.name = "grace";
  grace.value = "24h";
  grace.required = true;
  label.append(grace);
  const submit = element("button", "", "Preview garbage collection");
  submit.type = "submit";
  form.append(label, submit);
  const result = element("div");
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    previewGarbageCollection(result, grace.value).catch((error) => {
      showNotice(`Garbage collection preview failed: ${error.message}`, true);
    });
  });
  gcPanel.body.append(form, result);
  return gcPanel.container;
}

async function previewGarbageCollection(container, grace) {
  const result = await runGarbageCollection(true, grace);
  const actionBar = element("div", "button-row");
  actionBar.append(button("Apply this collection", () => applyGarbageCollection(grace), "danger"));
  container.replaceChildren(
    element("h3", "", "Deletion preview"),
    definitionList([
      ["Scanned", result.scanned],
      ["Referenced", result.referenced],
      ["Would delete", result.wouldDelete?.length || 0],
      ["Grace period", result.gracePeriod],
    ]),
  );
  const table = element("div", "preview-table");
  renderTable(table, [
    {label: "Unreferenced blob", render: (digest) => code(digest)},
  ], result.wouldDelete || [], {empty: "No blobs are eligible for deletion."});
  container.append(table, actionBar);
}

async function applyGarbageCollection(grace) {
  const accepted = await confirmAction(
    `Permanently delete every unreferenced blob older than ${grace}?`,
    {title: "Apply garbage collection", label: "Delete eligible blobs"},
  );
  if (!accepted) {
    return;
  }
  const result = await runGarbageCollection(false, grace);
  showNotice(
    `Garbage collection deleted ${result.deleted} blobs and reclaimed ${formatBytes(result.reclaimedBytes)}.`,
  );
  window.location.reload();
}

function runGarbageCollection(dryRun, grace) {
  const query = queryString({dryRun, grace});
  return api.json(`/api/v1/gc?${query}`, {method: "POST"});
}
