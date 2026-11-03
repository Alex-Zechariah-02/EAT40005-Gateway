import { MagnifyingGlass } from '@phosphor-icons/react'
import { useContext, useState, type ReactNode } from 'react'
import { Navigate, ViewMemory } from '../shared/hooks'

// The page header and the global search in it.

// PageHeader is the bar at the top of every view: its one short title (h1)
// with a one-line description of what the page is for beneath it, the
// view's actions, and the search.
export function PageHeader(props: { title: string; lede?: ReactNode; search?: boolean; children?: ReactNode }) {
  return (
    <header className="page-head">
      <div className="page-title">
        <h1 id="page-title" tabIndex={-1}>
          {props.title}
        </h1>
        {props.lede ? <p className="page-lede">{props.lede}</p> : null}
      </div>
      {props.children ? <div className="page-actions">{props.children}</div> : null}
      {props.search === false ? null : <GlobalSearch />}
    </header>
  )
}

// Each search hands Request lookup a new address, so a repeated search starts
// again; the counter carries no search term.
let searches = 0

// GlobalSearch finds what an administrator is given: a request's full Ref,
// an account ID, an endpoint ID, a draft change ("change 12"), a temporary
// rule ("rule 3") or a version ("version 4"). An account or endpoint goes to
// Request lookup through the page's memory, so it never enters the address
// or the browser history. It only opens pages; each page reads the gateway.
function GlobalSearch() {
  const [text, setText] = useState('')
  const [hint, setHint] = useState('')
  const memory = useContext(ViewMemory)
  const navigate = useContext(Navigate)
  const lookup = (field: 'subject' | 'operation', value: string) => {
    memory?.set('lookup', {
      form: { ref: '', subject: '', operation: '', decision: '', [field]: value },
      query: `${field}=${encodeURIComponent(value)}`,
    })
    searches += 1
    window.location.hash = `#/request-lookup?search=${searches}`
  }
  const find = (value: string) => {
    let match: RegExpExecArray | null
    if (/^[0-9a-f]{32}$/i.test(value)) window.location.hash = `#/request-lookup?ref=${encodeURIComponent(value.toLowerCase())}`
    else if (/^[0-9a-f]{4}-[0-9a-f]{4}$/i.test(value))
      return 'That is a short Ref. Ask for the full reference: the copy button beside a Ref copies all 32 characters.'
    else if ((match = /^(?:draft\s+)?change\s*#?(\d+)$/i.exec(value))) navigate?.('changes', { candidate: Number(match[1]) })
    else if (/^(?:temporary\s+)?rule\s*#?\d+$/i.test(value)) navigate?.('temporary-rules')
    else if (/^version\s*#?\d+$/i.test(value)) navigate?.('versions')
    else if (/^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+)+$/i.test(value)) lookup('operation', value)
    else if (/^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,63}$/.test(value)) lookup('subject', value)
    else return 'Search finds a full Ref, an account ID, an endpoint ID, “change 12”, “rule 3” or “version 4”.'
    return ''
  }
  return (
    <form
      className="ref-search"
      role="search"
      aria-label="Search the gateway"
      onSubmit={(event) => {
        event.preventDefault()
        const value = text.trim()
        if (value) setHint(find(value))
      }}
    >
      <MagnifyingGlass size={16} aria-hidden="true" />
      <input
        type="search"
        aria-label="Search"
        aria-describedby={hint ? 'search-hint' : undefined}
        placeholder="Ref, account, endpoint, change 12, rule 3"
        value={text}
        onChange={(event) => {
          setText(event.target.value)
          setHint('')
        }}
        maxLength={64}
        spellCheck={false}
        autoComplete="off"
      />
      {hint ? (
        <p id="search-hint" role="status" className="search-hint">
          {hint}
        </p>
      ) : null}
    </form>
  )
}
