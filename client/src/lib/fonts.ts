/*! Plus Jakarta Sans, copyright 2020 The Plus Jakarta Sans Project Authors. SIL Open Font License 1.1, notice in src/fonts/. */
import jakarta from '@fontsource-variable/plus-jakarta-sans/files/plus-jakarta-sans-latin-wght-normal.woff2?inline'

// The gateway's content policy has no font-src, so the browser refuses every
// font file and @font-face URL. The face therefore travels inside this
// first-party script and is registered with the CSS Font Loading API, which
// reads the font data from memory and fetches nothing. The app renders after
// it is registered, so text never changes face once it has appeared.

function fontData(dataUrl: string): ArrayBuffer {
  const binary = atob(dataUrl.slice(dataUrl.indexOf(',') + 1))
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return bytes.buffer
}

export async function loadFonts(): Promise<void> {
  const face = new FontFace('Plus Jakarta Sans', fontData(jakarta), { weight: '200 800' })
  await face.load()
  document.fonts.add(face)
}
