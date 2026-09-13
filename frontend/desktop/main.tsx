import React from 'react';
import { createRoot } from 'react-dom/client';
import OApp from '../app/OApp';
import '../app/globals.css';

const root = document.getElementById('root');
if (!root) throw new Error('Desktop root element is missing');

createRoot(root).render(
  <React.StrictMode>
    <OApp />
  </React.StrictMode>,
);
