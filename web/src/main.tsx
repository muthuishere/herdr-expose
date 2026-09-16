import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import './styles/app.css'
import { takeTokenFromUrl } from './net/auth'
import { connect, setTransportFactory } from './net/connection'
import { installViewportDriver } from './hooks/useViewport'

// Token from ?token= on first load, persisted, stripped from the URL (SPEC §8).
takeTokenFromUrl()

// Drives --app-height / --keyboard-inset from visualViewport (SPEC B8).
installViewportDriver()

// VITE_MOCK=1 swaps the transport only. Zero component changes when the real
// Go server lands.
if (import.meta.env.VITE_MOCK === '1' || import.meta.env.VITE_MOCK === 'true') {
  const { mockTransportFactory, installMockHttp } = await import('./mock/mockServer')
  setTransportFactory(mockTransportFactory)
  installMockHttp()
}

connect()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
