import type { Status } from './api'
import { Badge } from './ui'

// Brand names the console and the admin listener it is served from, so two
// gateways open side by side are never confused.
export function Brand() {
  return (
    <p className="brand">
      <span className="brand-mark" aria-hidden="true">
        F
      </span>
      <span className="brand-text">
        <span className="brand-name">Fetch Gateway</span>
        <span className="brand-host">{window.location.host}</span>
      </span>
    </p>
  )
}

// What the protected admission value means. Anything else is shown as sent.
export const admissionText: Record<string, string> = {
  ready: 'Protected requests are admitted under the effective policy.',
  bootstrap_locked: 'New installation: nothing is admitted until an administrator establishes the first policy (Changes).',
  recovery_required: 'Restored from a backup: nothing is admitted until an administrator establishes a recovery base (Changes).',
  audit_failed: 'The security event log cannot be written, so every protected request is refused.',
  generation_expired: 'The served policy passed a deadline; nothing is admitted until the gateway ends what expired.',
  stopping: 'The gateway is stopping and admits nothing.',
  stopped: 'The gateway has stopped and admits nothing.',
  unavailable: 'The gateway is unavailable and admits nothing.',
}

export function AdmissionBadge({ status }: { status: Status }) {
  return status.admission === 'ready' ? <Badge tone="ok">Admitting</Badge> : <Badge tone="refused">Not admitting: {status.admission}</Badge>
}
