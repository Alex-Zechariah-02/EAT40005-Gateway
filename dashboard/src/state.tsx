import type { Status } from './api'
import { Badge } from './ui'

// Brand names the console and its environment. The admin listener's exact
// address is one click away, so two gateways open side by side are never
// confused. "Local gateway" is shown only when it is served from this computer.
export function Brand() {
  const local = /^(127\.|localhost$|\[::1\]$)/.test(window.location.hostname)
  return (
    <div className="brand">
      <span className="brand-mark" aria-hidden="true">
        F
      </span>
      <span className="brand-text">
        <span className="brand-name">Fetch Gateway</span>
        <details className="brand-env">
          <summary>{local ? 'Local gateway' : window.location.hostname}</summary>
          <span className="brand-host">
            Admin address <code>{window.location.host}</code>
          </span>
        </details>
      </span>
    </div>
  )
}

// What the protected admission value means. Anything else is shown as sent.
export const admissionText: Record<string, string> = {
  ready: 'Protected requests are accepted under the policy in force.',
  bootstrap_locked: 'New installation: every request is refused until an administrator establishes the first policy (Changes).',
  recovery_required: 'Restored from a backup: every request is refused until an administrator establishes a starting policy after the restore (Changes).',
  audit_failed: 'The security log cannot be written, so every protected request is refused.',
  generation_expired: 'The policy in force passed a deadline; every request is refused until the gateway ends what expired.',
  stopping: 'The gateway is stopping and refuses every request.',
  stopped: 'The gateway has stopped and refuses every request.',
  unavailable: 'The gateway is unavailable and refuses every request.',
}

export function AdmissionBadge({ status }: { status: Status }) {
  return status.admission === 'ready' ? <Badge tone="ok">Accepting requests</Badge> : <Badge tone="refused">Not accepting requests</Badge>
}
