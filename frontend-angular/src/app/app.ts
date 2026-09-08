import { Component, signal } from '@angular/core';
import { BackendPanel } from './backend-panel/backend-panel';
import { BACKENDS } from './core/backends';
import { BackendDescriptor } from './core/ws.models';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [BackendPanel],
  templateUrl: './app.html',
  styleUrl: './app.css',
})
export class App {
  readonly backends = BACKENDS;
  readonly active = signal<BackendDescriptor>(BACKENDS[0]);

  select(b: BackendDescriptor): void {
    this.active.set(b);
  }
}
