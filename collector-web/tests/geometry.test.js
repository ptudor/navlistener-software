import test from 'node:test';
import assert from 'node:assert/strict';
import { sunDirection, site, elevation, modelFromFeed, assess, serverOffset, worldGrid } from '../src/map/geometry.js';

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
test('partial monitoring is distinct from no observations and complete coverage', () => {
  const second = { ...sv, name: 'G02' };
  const model = modelFromFeed(feed([sv, second], [observer(4)]), new Set([0]), at);
  const result = assess(model, 0, 0, 10, 2);
  assert.equal(result.expected, 2);
  assert.equal(result.observed, 1);
  assert.equal(result.missing, 1);
  assert.equal(result.status, 'thin');
  assert.equal(worldGrid(model, 10, 2, 10).coveredPercent, 0);
});
test('one station observing most of the local sky meets the practical target', () => {
  const satellites = Array.from({ length: 30 }, (_, i) => ({ ...sv, name: `G${i + 1}` }));
  const observations = satellites.slice(0, 28).map(s => ({ ...s, witness_times: [at / 1000] }));
  const envelope = feed(satellites, observations);
  let model = modelFromFeed(envelope, new Set([0]), at);
  let result = assess(model, 0, 0, 10, 1);
  assert.equal(result.observed, 28);
  assert.equal(result.missing, 2);
  assert.equal(result.status, 'covered');
  assert.ok(worldGrid(model, 10, 1, 10).coveredPercent > 0);
  // An unknown orbit must retain uncertainty without erasing positive evidence.
  envelope.data.reference.satellites.push({ name: 'G31', gnssid: 0 });
  model = modelFromFeed(envelope, new Set([0]), at);
  result = assess(model, 0, 0, 10, 1);
  assert.equal(result.status, 'unknown');
  assert.equal(result.observed, 28);
  assert.equal(result.uncertain, true);
  assert.equal(worldGrid(model, 10, 1, 10).coveredPercent, 0);
  // Loss of all fresh observations returns red; an absent sky stays unknown.
  model = modelFromFeed(envelope, new Set([0]), at + 61000);
  assert.equal(assess(model, 0, 0, 10, 1).status, 'gap');
  assert.equal(assess(model, 0, 180, 10, 1).status, 'unknown');
});
test('50% and 80% boundaries grade coverage without hiding individual gaps', () => {
  const satellites = Array.from({ length: 10 }, (_, i) => ({ ...sv, name: `G${i + 1}` }));
  for (const [count, status] of [[0, 'gap'], [4, 'gap'], [5, 'thin'], [7, 'thin'], [8, 'covered'], [9, 'covered'], [10, 'covered']]) {
    const observations = satellites.slice(0, count).map(s => ({ ...s, witness_times: [at / 1000] }));
    const model = modelFromFeed(feed(satellites, observations), new Set([0]), at);
    const result = assess(model, 0, 0, 10, 1, true);
    assert.equal(result.status, status, `${count} of 10 observed`);
    assert.equal(result.missing, 10 - count);
    assert.equal(result.observedFraction, count / 10);
    assert.equal(result.visible.length, 10);
    if (count >= 5) assert.equal(assess(model, 0, 0, 10, 2).status, 'thin', 'one-station data lacks redundancy, not observations');
  }
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
test('a sky with no located satellite is unmapped only when orbits are missing', () => {
  // Complete reference: the far side of the Earth simply has nothing selected overhead.
  const complete = modelFromFeed(feed([sv]), new Set([0]), at);
  assert.equal(assess(complete, 0, 180, 10, 1).unmapped, false);
  // An unknown orbit could be anywhere, so an empty local sky is unassessed, not clear.
  const missingOrbit = modelFromFeed(feed([sv, { name: 'G02', gnssid: 0 }]), new Set([0]), at);
  let result = assess(missingOrbit, 0, 180, 10, 1);
  assert.equal(result.status, 'unknown');
  assert.equal(result.unmapped, true);
  assert.equal(assess(missingOrbit, 0, 0, 10, 1).unmapped, false, 'a located satellite overhead is still assessed');
  assert.equal(assess(missingOrbit, 0, 0, 10, 1).status, 'gap');
  // A selected constellation absent from the reference leaves its sky unassessed too.
  const absent = modelFromFeed(feed([sv]), new Set([0, 2]), at);
  assert.equal(assess(absent, 0, 180, 10, 1).unmapped, true);
  // A stale snapshot locates nothing anywhere.
  const stale = modelFromFeed(feed([sv]), new Set([0]), at + 91000);
  result = assess(stale, 0, 0, 10, 1);
  assert.equal(result.unmapped, true);
  assert.ok(worldGrid(stale, 10, 1, 30).cells.every((cell) => cell.unmapped));
});
test('only a reference position is reported as extrapolated', () => {
  const coasted = { ...sv, extrapolated: true };
  const envelope = feed([coasted]);
  envelope.data.reference.status = 'delayed';
  let model = modelFromFeed(envelope, new Set([0]), at);
  assert.equal(model.satellites[0].extrapolated, true);
  assert.deepEqual(model.satellites[0].position, sv.ecef_m);
  assert.equal(assess(model, 0, 0, 10, 1).status, 'gap', 'an extrapolated missing satellite keeps its gap');
  // A fresh local orbit supersedes the extrapolated reference position.
  envelope.data.observations = [{ ...observer(1), ecef_m: [26560000, 1000, 0] }];
  model = modelFromFeed(envelope, new Set([0]), at);
  assert.equal(model.satellites[0].extrapolated, false);
  assert.deepEqual(model.satellites[0].position, [26560000, 1000, 0]);
  // Without any position there is nothing extrapolated to report.
  model = modelFromFeed(feed([{ name: 'G02', gnssid: 0, extrapolated: true }]), new Set([0]), at);
  assert.equal(model.satellites[0].extrapolated, false);
});
test('staleness and witness ageing follow the collector clock from the Date header', () => {
  const header = new Date(at).toUTCString();
  const fastBrowser = at + 5 * 60000;
  assert.equal(serverOffset(header, fastBrowser), -5 * 60000);
  assert.equal(serverOffset(null, fastBrowser), null);
  assert.equal(serverOffset('soon', fastBrowser), null);
  const envelope = feed([sv], [observer(1)]);
  assert.equal(modelFromFeed(envelope, new Set([0]), fastBrowser).stale, true);
  const corrected = modelFromFeed(envelope, new Set([0]), fastBrowser + serverOffset(header, fastBrowser));
  assert.equal(corrected.stale, false);
  assert.equal(corrected.satellites[0].witnesses, 1);
  assert.equal(assess(corrected, 0, 0, 10, 1).status, 'covered');
});
test('an unparsable reference download time or unknown status cannot break the model', () => {
  const envelope = feed([sv]);
  envelope.data.reference.fetched_at = 'soon';
  envelope.data.reference.status = 'brand-new';
  const model = modelFromFeed(envelope, new Set([0]), at);
  assert.equal(model.reference.fetchedAt, null);
  assert.equal(model.reference.status, 'unavailable');
  assert.equal(model.uncertain, true);
  envelope.data.reference.fetched_at = new Date(at).toISOString();
  envelope.data.reference.status = 'current';
  const current = modelFromFeed(envelope, new Set([0]), at);
  assert.equal(current.reference.fetchedAt, at);
  assert.equal(current.reference.status, 'current');
  assert.equal(modelFromFeed(feed([sv]), new Set([0]), at).reference.fetchedAt, null, 'an omitted download time is simply pending');
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
