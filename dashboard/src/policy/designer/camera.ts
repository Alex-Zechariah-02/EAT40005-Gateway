import { useEffect, useState, type RefObject } from 'react'
import type { FitViewOptions, ReactFlowInstance } from '@xyflow/react'

// The canvas view: its height in the window, fitting the whole diagram, or one card at a readable size.

// The top keeps room for the canvas toolbar, so no card sits under it.
const fit: { maxZoom: number; padding: FitViewOptions['padding'] } = { maxZoom: 1, padding: { top: '64px', x: '6%', bottom: '6%' } }
// A card brought into focus is never shown smaller than this, so its text
// stays readable.
const readable = 0.85
// Camera moves ease in and out over a calm, fixed time; with reduced motion
// the view changes at once.
export function motion(ms: number): number {
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 0 : ms
}
export const ease = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - (-2 * t + 2) ** 3 / 2)
// fitTo shows the whole diagram in the window, clear of the toolbar and of
// the side form when it is open.
export function fitTo(flow: ReactFlowInstance, canvas: HTMLDivElement | null, duration: number) {
  const panel = canvas?.closest('.designer-workspace')?.querySelector('.designer-side-panel')
  const covered = canvas && panel && panel.getBoundingClientRect().top < canvas.getBoundingClientRect().bottom - 1
  const padding: FitViewOptions['padding'] = covered ? { top: '64px', bottom: '24px', left: '24px', right: `${panel.getBoundingClientRect().width + 36}px` } : fit.padding
  void flow.fitView({ ...fit, padding, duration, ease })
}
// allReadable says whether fitting every card keeps them at a readable size.
export function allReadable(flow: ReactFlowInstance, canvas: HTMLDivElement | null): boolean {
  if (!canvas) return true
  const bounds = flow.getNodesBounds(flow.getNodes())
  return Math.min((canvas.clientWidth * 0.88) / bounds.width, (canvas.clientHeight - 64 - 24) / bounds.height) >= readable
}
// focusOn brings one card or row (in the diagram's own coordinates) into
// the uncovered part of the canvas at a readable size: centred when it
// fits, its top just under the toolbar when it is taller than the window.
export function focusOn(flow: ReactFlowInstance, canvas: HTMLDivElement | null, box: { x: number; y: number; width: number; height: number }, duration: number) {
  if (!canvas) return
  const panel = canvas.closest('.designer-workspace')?.querySelector('.designer-side-panel')
  const canvasBox = canvas.getBoundingClientRect()
  const covered = panel && panel.getBoundingClientRect().top < canvasBox.bottom - 1
  const left = 24
  const right = covered ? panel.getBoundingClientRect().width + 36 : 24
  const top = 64
  const bottom = 24
  const width = canvasBox.width - left - right
  const height = canvasBox.height - top - bottom
  const zoom = Math.max(readable, Math.min(1, width / box.width, height / box.height))
  const x = left + (width - box.width * zoom) / 2 - box.x * zoom
  const y = box.height * zoom <= height ? top + (height - box.height * zoom) / 2 - box.y * zoom : top - box.y * zoom
  void flow.setViewport({ x, y, zoom }, { duration, ease })
}

// The canvas takes the window's height left between the toolbar above it
// and what follows it, so the whole diagram shows without scrolling the
// page. Full screen and narrow screens keep their own fixed heights.
export function useCanvasHeight(area: RefObject<HTMLDivElement | null>, expanded: boolean): number | undefined {
  const [height, setHeight] = useState<number>()
  useEffect(() => {
    if (expanded) {
      setHeight(undefined)
      return
    }
    const page = document.getElementById('content')
    const measure = () => {
      const canvas = area.current
      if (!canvas || !page || window.innerWidth < 768) {
        setHeight(undefined)
        return
      }
      const box = canvas.getBoundingClientRect()
      // The lowest element in the page's flow: closed dialogs and the fixed
      // notification bar take no room in it.
      const inFlow = [...page.children].filter(
        (child) => child.tagName !== 'DIALOG' && child.getClientRects().length && !['fixed', 'absolute'].includes(getComputedStyle(child).position),
      )
      const end = Math.max(box.bottom, ...inFlow.map((child) => child.getBoundingClientRect().bottom))
      const below = end - box.bottom + parseFloat(getComputedStyle(page).paddingBottom)
      setHeight(Math.max(420, Math.floor(window.innerHeight - (box.top + window.scrollY) - below)))
    }
    measure()
    const observer = new ResizeObserver(measure)
    if (page) observer.observe(page)
    window.addEventListener('resize', measure)
    return () => {
      observer.disconnect()
      window.removeEventListener('resize', measure)
    }
  }, [expanded])
  return height
}
