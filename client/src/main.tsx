import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { CSPProvider } from '@base-ui/react/csp-provider'
import './styles.css'
import './lib/theme'
import { App } from './app/app'
import { loadFonts } from './lib/fonts'

async function start() {
  // Development only: `npm run dev:mock` answers API calls from src/dev. The
  // condition is false in every other build, so the stand-in is left out.
  if (import.meta.env.MODE === 'mock') {
    const { installMock } = await import('./dev/mock')
    installMock()
  }
  // The page waits for its typeface so nothing reflows after it appears.
  // Should it fail to register, the page still renders in the fallback face.
  await loadFonts().catch((error: unknown) => console.error('The Fetch typeface could not be registered:', error))
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      {/* The gateway's policy forbids inline <style> elements; Base UI must
          style everything through the app's own classes. */}
      <CSPProvider disableStyleElements>
        <App />
      </CSPProvider>
    </StrictMode>,
  )
}

void start()
