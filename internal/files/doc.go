// Package files stores attachments and generated files and hands out scoped,
// opaque references to them (master plan 14.1, 16.2 and 19; task M3-06).
//
// # The reference
//
// A file is known to a client, an SDK and a model only by its FileRef
// (schemas/conversation/file-ref): an opaque id ("fil_" plus a ULID), a sanitised
// display name, the sniffed media type, the size, the owner, the origin, the
// treatment, the permissions the caller holds, whether a safe previewer exists
// and when the reference expires. It carries no path, no URL and no storage key,
// and nothing in this package ever hands one out: bytes are reached through
// Service.Open, which returns a reader. Every use of a reference (Stat, Open,
// Preview, Delete, ForModel) resolves it from scratch and re-checks the scope,
// the principal, the session, the expiry and the permission; nothing is cached
// (SEC-026). A reference that does not resolve in the caller's scope is always the
// same scope_violation, whether the file is another tenant's, another principal's,
// deleted or never existed, so a reference cannot be used to probe for files.
//
// # Policy: what an attachment is
//
// The host application declares how it treats attachments (Policy, from the
// "files" configuration or a PolicySource): ephemeral turn context, an explicitly
// registered RAG source, or an application object, each with the media types it
// accepts, a size cap, a lifetime, the permissions it grants and who in the
// workspace may use it. A treatment with no enabled rule is refused, and so is a
// request for one the policy does not name (permission_denied); the client and
// the model never choose. By default only ephemeral_context is on: readable and
// previewable by its uploader, inside the session that uploaded it, for an hour.
// A rag_source file is stored and then handed to the RAG pipeline through the
// RAGSources seam, and withdrawn from it before it is ever tombstoned.
//
// # What the bytes must be
//
// The declared media type must be one the core can verify, one the policy
// allows, and what the first bytes sniff as (http.DetectContentType): a PNG
// declared as text, a page of HTML declared as PNG, a ZIP declared as an Office
// document without its package structure are all refused and nothing is stored
// (SEC-019). HTML, SVG, XML and script are not in the table at all. Previews
// exist only for raster images and plain text, and are served with a sandboxing
// Content-Security-Policy; everything else is a download. A model is given text
// (bounded) and images only when a vision-capable provider is installed
// (ForModel); never a path.
//
// # Storage and SEC-023
//
// Bytes are content-addressed files below <data_dir>/files. The address is a
// function of the scope and the content: a blob's name is SHA-256(application,
// tenant, workspace, SHA-256(content)) and it lives in a directory named by a
// hash of the application and tenant. Identical uploads by different tenants (or
// workspaces) are different files with different names and different ids, and
// dedupe happens only inside one scope, so one tenant can never learn that
// another holds the same content. No scope identifier is ever part of a path.
// Directories are created 0700 and files 0600 (SEC-081), and every operation goes
// through an os.Root that refuses any path leaving the directory.
//
// # Quota and deletion
//
// A tenant (across its workspaces) has a live-file and a live-byte quota
// (resource_exhausted beyond it). Deleting a file, the expiry of a reference and
// deleting a tenant (DeleteTenant) are tombstones: the file stops resolving at
// once. Compact, run in the background (RunCompaction), then removes the blob
// when no live row of the same scope uses it, and then the row (master plan 19).
// Compact and DeleteTenant are cross-tenant by nature and are not part of the
// gateway's tenant-facing interface.
//
// # Size over HTTP
//
// The gateway applies gateway.max_body_bytes to every request before a route runs
// (64 KiB by default, 16 MiB at most), so over the API a file cannot be larger
// than that body limit; files.max_file_bytes and the rule's max_bytes only
// narrow it further.
package files
