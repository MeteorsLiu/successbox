import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { ToastProvider } from './components/Toast';
import { TrustedWindow } from './trusted/TrustedWindow';
import './styles.css';

// Both windows load this page; the trusted window is opened with #/trusted.
const trusted = window.location.hash === '#/trusted';
document.title = 'Successbox';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ToastProvider>{trusted ? <TrustedWindow /> : <App />}</ToastProvider>
  </StrictMode>,
);
