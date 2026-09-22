const { contextBridge, ipcRenderer } = require('electron');

const prefix = '--o-api-origin=';
const rawOrigin = process.argv.find((value) => value.startsWith(prefix))?.slice(prefix.length) ?? '';
const apiOrigin = /^http:\/\/127\.0\.0\.1:\d+$/.test(rawOrigin) ? rawOrigin : '';

contextBridge.exposeInMainWorld('oDesktop', Object.freeze({
  apiOrigin,
  platform: process.platform,
  version: process.versions.electron,
  copyText: (text) => ipcRenderer.invoke('o-clipboard-write', text),
  selectDirectory: (options) => ipcRenderer.invoke('o-select-directory', options),
}));
