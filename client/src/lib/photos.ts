// One royalty-free photo per merchant and item, named by ID, bundled with the
// app so the content policy's img-src 'self' holds. Credits are in
// src/photos/CREDITS.md.
const files = import.meta.glob<string>('../photos/*.webp', { eager: true, query: '?url', import: 'default' })

const byId = new Map(Object.entries(files).map(([path, url]) => [path.slice(path.lastIndexOf('/') + 1, -'.webp'.length), url]))

// photoUrl returns the photo for an ID, or null for an ID without one; the
// caller shows a neutral placeholder instead.
export function photoUrl(id: string): string | null {
  return byId.get(id) ?? null
}
