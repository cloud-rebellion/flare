import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { parse } from 'svelte/compiler';

const page = readFileSync(new URL('../src/routes/issues/[id]/+page.svelte', import.meta.url), 'utf8');

const eachBlocks = (node, found = []) => {
  if (Array.isArray(node)) {
    for (const child of node) eachBlocks(child, found);
  } else if (node && typeof node === 'object') {
    if (node.type === 'EachBlock') found.push(node);
    for (const child of Object.values(node)) eachBlocks(child, found);
  }
  return found;
};

test('the issue page does not key event tags on client-written text', () => {
  const ast = parse(page, { modern: true });
  const tagBlocks = eachBlocks(ast.fragment).filter((block) =>
    page.slice(block.expression.start, block.expression.end).includes('tags')
  );
  expect(tagBlocks.length).toBe(1);
  expect(tagBlocks[0].key ?? null).toBeNull();
});
