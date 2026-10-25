package sdktest

import "time"

// The files of the fake, as far as a turn needs them: the SDK has no upload (a host uploads through POST /v1/files, or
// with the TypeScript SDK), so a test registers the files the host's users "uploaded" and then names their ids in a turn.
// A file resolves in a turn only for the caller it was registered for (application, tenant, workspace, principal and
// session), and only while it exists and has not expired. Every other case (another tenant's, principal's or session's
// file, a deleted one, an expired one, one that never existed) is the same 403 scope_violation, so a reference cannot be
// used to find out which files exist.

const fileTTL = 15 * time.Minute

type fileEntry struct {
	owner   Claims
	expires time.Time
	deleted bool
}

// AddFile registers a file uploaded by the caller these claims describe and returns its id (fil_ and a ULID). It lives
// 15 minutes on the fake's clock.
func (f *Fake) AddFile(owner Claims) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.typedID("fil")
	f.files[id] = &fileEntry{owner: owner.withDefaults(), expires: f.now().Add(fileTTL)}
	return id
}

// DeleteFile deletes a file: it stops resolving at once.
func (f *Fake) DeleteFile(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.files[id]; e != nil {
		e.deleted = true
	}
}

// ExpireFile makes a file expire now, whatever the clock says.
func (f *Fake) ExpireFile(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.files[id]; e != nil {
		e.expires = f.now()
	}
}

// attachable reports whether every file id resolves for the caller. The caller holds f.mu.
func (f *Fake) attachable(caller Claims, ids []string) bool {
	for _, id := range ids {
		e := f.files[id]
		if e == nil || e.deleted || !f.now().Before(e.expires) || !sameScope(e.owner, caller) {
			return false
		}
	}
	return true
}
