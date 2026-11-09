/**
 * Registry of chat games. To add a game: create a module exporting
 * `meta` ({ id, name, icon }) and a factory matching the contract
 * `create({ onClose }) -> { element, focus, destroy }`, then add one line here.
 */

import { createGame2048, meta as meta2048 } from './game-2048.js';
import { createSnake, meta as metaSnake } from './game-snake.js';
import { createMemory, meta as metaMemory } from './game-memory.js';
import { createMinesweeper, meta as metaMines } from './game-minesweeper.js';
import { createLightsOut, meta as metaLights } from './game-lightsout.js';
import { createSimon, meta as metaSimon } from './game-simon.js';

// Per-game best-score persistence, shared by every game module. Each game
// passes its own storage key; storage may be unavailable (private mode,
// blocked cookies), so both sides swallow failures.
export function readBest(key) {
  try { return parseInt(localStorage.getItem(key), 10) || 0; } catch { return 0; }
}

export function writeBest(key, v) {
  try { localStorage.setItem(key, String(v)); } catch { /* storage may be off */ }
}

export const GAMES = [
  { ...meta2048, create: createGame2048 },
  { ...metaSnake, create: createSnake },
  { ...metaMemory, create: createMemory },
  { ...metaMines, create: createMinesweeper },
  { ...metaLights, create: createLightsOut },
  { ...metaSimon, create: createSimon },
];
