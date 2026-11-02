import {
  Timer,
  ClockCounterClockwise,
  ShieldCheck,
  SquaresFour,
  Pulse,
  MagnifyingGlass,
  Plugs,
  GitDiff,
  UserMinus,
  UsersThree,
  UserCircle,
  Stack,
  Scroll,
  Key,
  Gauge,
  Lightbulb,
} from '@phosphor-icons/react'

// The navigation's icons, one per view.
const icons = {
  overview: SquaresFour,
  traffic: Pulse,
  lookup: MagnifyingGlass,
  apis: Plugs,
  accounts: UserMinus,
  candidates: GitDiff,
  overrides: Timer,
  history: ClockCounterClockwise,
  team: UsersThree,
  account: UserCircle,
  permit: ShieldCheck,
  versions: Stack,
  log: Scroll,
  access: Key,
  limits: Gauge,
  advice: Lightbulb,
}
export type IconName = keyof typeof icons

export function Icon({ name }: { name: IconName }) {
  const Component = icons[name]
  return <Component className="icon" size={20} weight="regular" aria-hidden="true" focusable="false" />
}
