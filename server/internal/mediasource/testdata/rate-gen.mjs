// Гоним НАСТОЯЩИЙ throughput@1.0.1 по детерминированной последовательности:
// Date.now подменён, поэтому результат воспроизводим и годится как эталон.
import { createRequire } from 'node:module';
let fakeNow = 1700000000000;
Date.now = () => fakeNow;
const require = createRequire(import.meta.url);
const throughput = require('/tmp/package/index.js');

const meter = throughput(5);
// Последовательность специально проходит все ветки: разогрев буфера (len < 10),
// заполнение окна (len < size), устойчивый поток, простой длиннее окна.
const steps = [];
const script = [
  ...Array.from({length: 12}, () => [100, 1000]),   // разогрев по тику
  ...Array.from({length: 40}, () => [100, 5000]),   // окно заполняется
  ...Array.from({length: 20}, () => [100, 5000]),   // устойчивый поток
  [3000, 0],                                        // простой длиннее окна
  ...Array.from({length: 10}, () => [100, 100]),    // возобновление
  [50, 700],                                        // подтик: время не выросло
  [100, 0],
];
for (const [advance, delta] of script) {
  fakeNow += advance;
  steps.push({ advance, delta, rate: meter(delta) });
}
console.log(JSON.stringify(steps));
