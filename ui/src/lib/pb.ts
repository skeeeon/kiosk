import PocketBase, { BaseAuthStore } from 'pocketbase'

// The kiosk is a shared physical device. Admin tokens must not survive a tab
// close or browser refresh, so we extend BaseAuthStore (no persistence) and
// avoid the default LocalAuthStore (localStorage).
//
// BaseAuthStore is declared abstract in the types but has no abstract members;
// an empty subclass is the minimal way to instantiate it cleanly under TS.
class MemoryAuthStore extends BaseAuthStore {}

export const pb = new PocketBase('/', new MemoryAuthStore())
