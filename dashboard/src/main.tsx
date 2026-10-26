import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './styles.css'
import './designer.css'
import './theme'
import { App } from './app'
import { loadFonts } from './fonts'

function render() {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  )
}

// The page waits for its typefaces so that nothing reflows after it appears.
// Should they fail to register, it still renders, in the browser's own faces.
loadFonts().then(render, (error: unknown) => {
  console.error('The interface typefaces could not be registered:', error)
  render()
})
