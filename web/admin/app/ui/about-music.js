/**
 * Generative keygen-style chiptune for the About dialog, synthesized live via
 * Web Audio (pulse lead, triangle bass, noise drums). A lookahead scheduler
 * queues notes ~120ms ahead on the audio clock so main-thread hitches don't
 * break the groove; missing Web Audio or blocked autoplay is a silent no-op.
 */

const BPM = 150;
const STEP_DUR = 60 / BPM / 4;   // one sixteenth note, in seconds
const STEPS_PER_BAR = 16;
const LOOKAHEAD = 0.12;          // schedule this far ahead of the audio clock (s)
const TIMER_MS = 25;             // how often the scheduler wakes

// i - VI - III - VII in A minor: Am, F, C, G. Each is a triad as MIDI notes;
// one chord per bar, four-bar loop.
const PROGRESSION = [
  [57, 60, 64], // Am  (A3 C4 E4)
  [53, 57, 60], // F   (F3 A3 C4)
  [48, 52, 55], // C   (C3 E3 G3)
  [55, 59, 62], // G   (G3 B3 D4)
];

const midiToFreq = (m) => 440 * Math.pow(2, (m - 69) / 12);

/**
 * Create an About-dialog chiptune player.
 * @returns {{ start: Function, stop: Function }} idempotent controls.
 */
export function createAboutMusic() {
  const AC = window.AudioContext || window.webkitAudioContext;
  let ctx = null;
  let master = null;       // fade-in/out stage feeding the compressor
  let noiseBuffer = null;  // shared white-noise buffer for the drums
  let timer = null;
  let step = 0;            // absolute sixteenth counter
  let nextTime = 0;        // audio-clock time of the next step
  let running = false;

  // A short enveloped oscillator note (lead / bass).
  function tone(freq, at, dur, peak, type) {
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = type;
    osc.frequency.value = freq;
    gain.gain.setValueAtTime(0.0001, at);       // exp ramps can't touch zero
    gain.gain.exponentialRampToValueAtTime(peak, at + 0.008);
    gain.gain.exponentialRampToValueAtTime(0.0001, at + dur);
    osc.connect(gain).connect(master);
    osc.start(at);
    osc.stop(at + dur + 0.02);
  }

  // Kick: a sine with a fast downward pitch sweep.
  function kick(at) {
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.setValueAtTime(140, at);
    osc.frequency.exponentialRampToValueAtTime(45, at + 0.12);
    gain.gain.setValueAtTime(0.0001, at);
    gain.gain.exponentialRampToValueAtTime(0.5, at + 0.005);
    gain.gain.exponentialRampToValueAtTime(0.0001, at + 0.18);
    osc.connect(gain).connect(master);
    osc.start(at);
    osc.stop(at + 0.2);
  }

  // Hi-hat: a short burst of high-passed white noise.
  function hat(at, peak) {
    const src = ctx.createBufferSource();
    src.buffer = noiseBuffer;
    const hp = ctx.createBiquadFilter();
    hp.type = 'highpass';
    hp.frequency.value = 7000;
    const gain = ctx.createGain();
    gain.gain.setValueAtTime(peak, at);
    gain.gain.exponentialRampToValueAtTime(0.0001, at + 0.04);
    src.connect(hp).connect(gain).connect(master);
    src.start(at);
    src.stop(at + 0.05);
  }

  // Schedule everything that lands on one sixteenth-note step.
  function scheduleStep(absStep, at) {
    const barStep = absStep % STEPS_PER_BAR;
    const chord = PROGRESSION[Math.floor(absStep / STEPS_PER_BAR) % PROGRESSION.length];

    // Lead: the keygen signature. Rip up the chord tones across two octaves,
    // one bright pulse note per sixteenth, so the chord reads as a fast arp.
    const arp = [chord[0], chord[1], chord[2], chord[0] + 12, chord[1] + 12, chord[2] + 12];
    const lead = arp[barStep % arp.length] + 12; // up an octave for chiptune sparkle
    tone(midiToFreq(lead), at, STEP_DUR * 0.9, 0.14, 'square');

    // Bass: driving triangle root on every eighth note.
    if (barStep % 2 === 0) {
      tone(midiToFreq(chord[0] - 12), at, STEP_DUR * 1.8, 0.22, 'triangle');
    }

    // Drums: four-on-the-floor kick, offbeat hats (accented on the beat's "and").
    if (barStep % 4 === 0) kick(at);
    if (barStep % 2 === 1) hat(at, barStep % 4 === 3 ? 0.09 : 0.05);
  }

  // Lookahead scheduler: queue every step that falls inside the lookahead window.
  function scheduler() {
    if (!running || !ctx) return;
    while (nextTime < ctx.currentTime + LOOKAHEAD) {
      scheduleStep(step, nextTime);
      nextTime += STEP_DUR;
      step += 1;
    }
  }

  function start() {
    if (running || !AC) return;
    try {
      ctx = new AC();

      // Glue bus: master fade -> compressor -> speakers, so stacked voices stay
      // punchy without clipping.
      master = ctx.createGain();
      master.gain.value = 0.0001;
      const comp = ctx.createDynamicsCompressor();
      master.connect(comp);
      comp.connect(ctx.destination);

      // One-shot white-noise buffer reused by every hat.
      noiseBuffer = ctx.createBuffer(1, ctx.sampleRate * 0.2, ctx.sampleRate);
      const data = noiseBuffer.getChannelData(0);
      for (let i = 0; i < data.length; i++) data[i] = Math.random() * 2 - 1;

      if (ctx.state === 'suspended') ctx.resume();

      // Gentle 1s fade-in so it swells in rather than popping.
      master.gain.setValueAtTime(0.0001, ctx.currentTime);
      master.gain.exponentialRampToValueAtTime(0.5, ctx.currentTime + 1);

      running = true;
      step = 0;
      nextTime = ctx.currentTime + 0.1;
      scheduler();
      timer = setInterval(scheduler, TIMER_MS);
    } catch (_) {
      running = false;
    }
  }

  function stop() {
    if (!running) return;
    running = false;
    if (timer) { clearInterval(timer); timer = null; }
    const closing = ctx;
    if (closing && master) {
      try {
        const t = closing.currentTime;
        master.gain.cancelScheduledValues(t);
        master.gain.setValueAtTime(master.gain.value, t);
        master.gain.exponentialRampToValueAtTime(0.0001, t + 1); // 1s fade out
      } catch (_) { /* fall through to close */ }
      setTimeout(() => { try { closing.close(); } catch (_) {} }, 1100);
    }
    ctx = null;
    master = null;
    noiseBuffer = null;
  }

  return { start, stop };
}
