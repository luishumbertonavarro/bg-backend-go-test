import { BackendDescriptor } from './ws.models';

/** Puerto según SECURITY-CHECKLIST.md §0. Editable en la UI. */
export const BACKENDS: BackendDescriptor[] = [
  {
    id: 'go',
    label: 'Go',
    defaultUrl: 'ws://localhost:8084/ws',
    hint: 'net/http + gorilla/websocket',
  },
];
