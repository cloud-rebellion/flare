import { expect, test } from 'bun:test';
import { scrubFlareBrowserPath } from '../src/lib/flare-client';

test('browser paths retain route shape without arbitrary tokens', () => {
	expect(scrubFlareBrowserPath('/sign/opaqueInviteToken')).toBe('/sign/:id');
	expect(scrubFlareBrowserPath('/sign/opaque%2FInviteToken')).toBe('/sign/:id/:id');
	expect(scrubFlareBrowserPath('/users/alice%40example.com')).toBe('/users/:id');
	expect(scrubFlareBrowserPath('/api/projects/customer-name/issues/12345')).toBe('/api/projects/:id/issues/:id');
	expect(scrubFlareBrowserPath('/_app/immutable/chunks/app.BhOPzaDi.js')).toBe('/_app/immutable/chunks/:id');
});

test('browser route templates are bounded', () => {
	const route = scrubFlareBrowserPath('/' + 'secret/'.repeat(100), 40);
	expect(route.length).toBeLessThanOrEqual(40);
	expect(route).not.toContain('secret');
});
