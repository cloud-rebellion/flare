import { expect, test } from 'bun:test';
import { formatExactQuantity } from '../src/lib/format';

test('counter formatting preserves an integer above Number.MAX_SAFE_INTEGER', () => {
  expect(formatExactQuantity('9007199254740993', 'en-US')).toBe('9,007,199,254,740,993');
});

test('counter formatting retains zero', () => {
  expect(formatExactQuantity('0', 'en-US')).toBe('0');
});

test('counter formatting rejects nondecimal counter strings', () => {
  for (const value of ['', '-1', '1.2', '9e3', '1,000', ' 12']) {
    expect(formatExactQuantity(value, 'en-US')).toBe('Unavailable');
  }
});
