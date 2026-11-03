import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './styles/tokens.css'
import './styles/base.css'
import './styles/names.css'
import './styles/controls.css'
import './styles/forms.css'
import './styles/content.css'
import './styles/notices.css'
import './styles/dialogs.css'
import './styles/toggles.css'
import './styles/signin.css'
import './styles/page.css'
import './styles/layout.css'
import './styles/lists.css'
import './styles/overview.css'
import './styles/traffic.css'
import './styles/wizard.css'
import './styles/choices.css'
import './styles/requests.css'
import './styles/policy.css'
import './styles/links.css'
import './styles/changes.css'
import './styles/outcomes.css'
import './styles/versions.css'
import './styles/administration.css'
import './styles/access.css'
import './styles/narrow.css'
import './styles/sidebar.css'
import './styles/overlays.css'
import './policy/designer/designer.css'
import './policy/designer/views.css'
import './policy/designer/canvas.css'
import './policy/designer/matrix.css'
import './app/theme'
import { App } from './app/app'
import { loadFonts } from './app/fonts'

// Starts the Dashboard once its fonts are ready.

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
