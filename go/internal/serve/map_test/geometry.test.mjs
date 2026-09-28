import test from 'node:test';
import assert from 'node:assert/strict';
import { sunDirection, site, elevation, modelFromFeed, assess, worldGrid } from '../map/geometry.mjs';

const at = Date.parse('2026-09-22T12:00:00Z');
function feed(satellites, observations = []) {
  return { ok: true, time: new Date(at).toISOString(), data: { audience: 'public', reference: { status: 'current', satellites }, observations } };
}
const sv = { name: 'G01', gnssid: 0, ecef_m: [26560000, 0, 0] };
const observer = (count, age = 0) => ({ ...sv, witness_times: Array(count).fill(at / 1000 - age) });

test('reference-only satellites reveal gaps before any receiver observes them', () => {
  const model = modelFromFeed(feed([sv]), new Set([0]), at);
  assert.equal(assess(model, 0, 0, 10, 1).status, 'gap');
  assert.equal(assess(model, 0, 180, 10, 1).expected, 0);
});
test('one missing satellite cannot be hidden by many observed satellites', () => {
  const second = { ...sv, name: 'G02' };
  const model = modelFromFeed(feed([sv, second], [observer(4)]), new Set([0]), at);
  assert.equal(assess(model, 0, 0, 10, 2).missing, 1);
  assert.equal(assess(model, 0, 0, 10, 2).status, 'gap');
});
test('redundancy and receipt expiry are independent of orbit availability', () => {
  const envelope = feed([sv], [observer(1)]);
  const model = modelFromFeed(envelope, new Set([0]), at);
  assert.equal(assess(model, 0, 0, 10, 1).status, 'covered');
  assert.equal(assess(model, 0, 0, 10, 2).status, 'thin');
  const expired = modelFromFeed(envelope, new Set([0]), at + 61000);
  assert.equal(assess(expired, 0, 0, 10, 1).status, 'gap');
  const stale = modelFromFeed(envelope, new Set([0]), at + 91000);
  assert.equal(assess(stale, 0, 0, 10, 1).status, 'unknown');
});
test('unknown orbit, missing constellation and failed reference cannot look complete', () => {
  const missingOrbit = { name: 'G02', gnssid: 0 };
  for (const [envelope, selected] of [
    [feed([sv, missingOrbit], [observer(2)]), new Set([0])],
    [feed([sv], [observer(2)]), new Set([0, 2])],
    [{ ...feed([sv], [observer(2)]), data: { ...feed([sv]).data, reference: { status: 'delayed', satellites: [sv] }, observations: [observer(2)] } }, new Set([0])],
  ]) {
    const model = modelFromFeed(envelope, selected, at);
    assert.equal(assess(model, 0, 0, 10, 2).status, 'unknown');
    assert.equal(worldGrid(model, 10, 2, 10).coveredPercent, 0);
  }
});
test('dateline, poles and elevation use consistent WGS-84 geometry', () => {
  assert.ok(Math.abs(elevation(sv.ecef_m, site(0, 0)) - 90) < 1e-6);
  assert.ok(Math.abs(elevation([-26560000, 0, 0], site(0, -180)) - elevation([-26560000, 0, 0], site(0, 180))) < 1e-9);
  assert.ok(Math.abs(elevation([0, 0, 26560000], site(90, 123)) - 90) < 1e-6);
});
test('day/night boundary handles equinox and both polar seasons', () => {
  const equinox = sunDirection(new Date('2026-03-20T12:00:00Z'));
  assert.ok(equinox[0] > 0.99 && Math.abs(equinox[2]) < 0.01);
  const summer = sunDirection(new Date('2026-06-21T12:00:00Z'));
  const winter = sunDirection(new Date('2026-12-21T12:00:00Z'));
  assert.ok(summer[2] > 0.39 && summer[2] < 0.41);
  assert.ok(winter[2] < -0.39 && winter[2] > -0.41);
  for (const s of [equinox, summer, winter]) assert.ok(Math.abs(Math.hypot(...s) - 1) < 1e-12);
});
