// Loaded before every test file: jest-dom's matchers, and a DOM torn down
// between tests so one test's markup cannot be found by the next.
import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/svelte'
import { afterEach } from 'vitest'

// jsdom lays nothing out, so it has no scrollIntoView. Components that keep a
// highlighted row in view call it, and an unimplemented method would surface
// as an unhandled rejection from the tick they call it on.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {}
}

afterEach(() => {
  cleanup()
})
