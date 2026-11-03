import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './tokens.css'
import './base.css'
import './names.css'
import './controls.css'
import './forms.css'
import './content.css'
import './notices.css'
import './dialogs.css'
import './toggles.css'
import './signin.css'
import './page.css'
import './layout.css'
import './lists.css'
import './overview.css'
import './traffic.css'
import './wizard.css'
import './choices.css'
import './requests.css'
import './policy.css'
import './links.css'
import './changes.css'
import './outcomes.css'
import './versions.css'
import './administration.css'
import './access.css'
import './narrow.css'
import './sidebar.css'
import './overlays.css'
import './designer.css'
import './designer-list.css'
import './designer-cards.css'
import './designer-matrix.css'
import './theme'
import { App } from './app'
import { loadFonts } from './fonts'

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
