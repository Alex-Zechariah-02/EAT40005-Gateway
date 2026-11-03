/*! Hanken Grotesk, copyright The Hanken Grotesk Project Authors; Geist Mono, copyright 2024 The Geist Project Authors. SIL Open Font License 1.1, notices in src/fonts/. */
import hanken from '@fontsource-variable/hanken-grotesk/files/hanken-grotesk-latin-wght-normal.woff2?inline'
import mono from '@fontsource-variable/geist-mono/files/geist-mono-latin-wght-normal.woff2?inline'

// The gateway serves the dashboard under a content policy with no font-src,
// so the browser refuses every font file and @font-face URL. The faces
// therefore travel inside this first-party script and are registered with the
// CSS Font Loading API, which reads the font data from memory and fetches
// nothing. The page renders after they are registered, so text never changes
// face, and layout never shifts, once it has appeared.

// fontData turns a base64 data URL produced by the build into bytes.
function fontData(dataUrl: string): ArrayBuffer {
  const binary = atob(dataUrl.slice(dataUrl.indexOf(',') + 1))
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return bytes.buffer
}

export async function loadFonts(): Promise<void> {
  const faces = [
    new FontFace('Hanken Grotesk', fontData(hanken), { weight: '100 900' }),
    new FontFace('Geist Mono', fontData(mono), { weight: '100 900' }),
  ]
  await Promise.all(faces.map((face) => face.load()))
  for (const face of faces) document.fonts.add(face)
}
