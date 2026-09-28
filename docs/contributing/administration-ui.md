# Administration UI design guidelines

The embedded administration application is a small, dependency-free ES-module app. It
must remain usable directly from the files embedded by Go: no package installation,
bundler, runtime CDN, or generated assets are required.

## Navigation and resource shape

- Use hash URLs for application state. Every screen must survive reload and be
  shareable as a URL such as `#/repositories/releases/assets/42`.
- Follow list → detail → edit. Lists link to durable detail routes; detail headers expose
  every supported operation; create and edit use the shared descriptor-driven dialog.
- Do not add a control until the corresponding API operation exists. Read-only history
  is labelled read-only rather than given a fake retry, ping, or requeue button.
- Paginate long collections: use numbered pages for control-plane lists that expose
  `total`, and cursor navigation for artifact and task lists. Do not download an
  unbounded collection just to display its first screen. Selectors that need the
  complete collection, such as the attached cleanup-policy chooser, follow every
  cursor before filtering their options.
- Destructive operations require the shared confirmation dialog. Cleanup and garbage
  collection always use preview → review candidates → explicit apply.

## Forms and feedback

- Describe resource fields in `schemas.js` and render native text, number, boolean,
  select, list, key/value, checkbox, JSON, or predicate controls. Keep the Advanced JSON
  editor as an expert escape hatch, not the primary workflow. Switching back to fields
  applies valid JSON and retains writable keys without visible controls; invalid JSON
  keeps the editor open until corrected.
- Preserve predicate literal types when opening and saving forms. Display saved
  strings as quoted JSON so values such as `"true"` or `"42"` remain strings;
  users may also enter plain text for values that are not JSON literals.
- Download gates permit an explicit empty criteria array, including an inheritance
  opt-out. Cleanup policies still require at least one selection predicate.
- Put validation errors beside the affected field and request errors in the dialog's
  inline error area. Persistent page-level results use the single notice channel.
- Generated credentials are shown in the secret dialog exactly once and are never
  written to logs, URLs, or ordinary page content.
- Never call `window.prompt`, `window.confirm`, or `window.alert`.

## Security

- The token sign-in form stores a bootstrap or API token only in `sessionStorage` and
  sends it as a Bearer credential. Never copy it into a URL, cookie, `localStorage`, log,
  or rendered page content. OIDC sessions continue to use their HTTP-only cookie.
- Keep the server's strict same-origin Content Security Policy. Do not use inline script,
  inline event handlers, `eval`, remote fonts, remote styles, or remote script.
- Build DOM with `createElement`, `textContent`, and the shared component helpers. Never
  place API data into `innerHTML`.
- Build API and hash paths with the shared path helpers so each segment is encoded once.
- Validate Raw upload paths before passing them to Fetch: browsers normalize `.` and
  `..` segments before the server can enforce its literal-path contract.
- Hide actions that the `whoami` effective privilege list cannot authorize. Server-side
  authorization remains mandatory; the UI check is for clarity, not security.

## Accessibility and layout

- Every action must be keyboard reachable. Interactive table rows respond to Enter and
  Space and retain a visible focus outline.
- Use native labels, fieldsets, dialogs, buttons, details, and tables before custom
  interaction widgets. Dialogs must offer explicit cancel/close actions.
- Preserve readable source: one statement per line, descriptive names, and small focused
  functions. Do not commit generated or manually minified JavaScript.
- Support narrow screens without hiding operations. Tables may scroll horizontally;
  navigation and action bars wrap or scroll; edit rows collapse to one column.
- Support light, dark, and system-selected themes through the existing CSS variables.
  New colors must have equivalents in both palettes and retain visible focus, error,
  border, and text contrast.

## Adding a resource

1. Add its field descriptor and initial values to `schemas.js`.
2. Add its API metadata and table columns to `resource_views.js`, or a focused view
   module if its workflows are not ordinary CRUD.
3. Link every API operation from its detail screen and apply `hasPrivilege` to each
   action.
4. Add router/schema unit coverage and update the embedded-asset server test.
   `go test` also checks `schemas.js` field names against the mapped OpenAPI request
   schemas, and `webhookEvents` against the OpenAPI enum and domain event constants.
5. Run `make test-ui`, `make check`, and both server builds.
