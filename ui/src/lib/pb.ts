import PocketBase, { BaseAuthStore } from 'pocketbase'

// The kiosk is a shared physical device. Admin tokens must not survive a tab
// close or browser refresh, so we extend BaseAuthStore (no persistence) and
// avoid the default LocalAuthStore (localStorage).
//
// BaseAuthStore is declared abstract in the types but has no abstract members;
// an empty subclass is the minimal way to instantiate it cleanly under TS.
class MemoryAuthStore extends BaseAuthStore {}

export const pb = new PocketBase('/', new MemoryAuthStore())

// pbWorker authenticates the `users` collection on the public virtual
// timeclock terminal (cmd/timeclock). It deliberately uses PocketBase's
// DEFAULT LocalAuthStore (localStorage) rather than the admin client's
// MemoryAuthStore: the virtual terminal runs on a worker's PERSONAL phone, so
// the session SHOULD survive a reload — the opposite of the shared-kiosk
// requirement above. Separate instance so the two token lifetimes never mix.
// Idle on a regular kiosk/controller (no worker ever logs in there).
export const pbWorker = new PocketBase('/')
