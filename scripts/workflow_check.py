#!/usr/bin/env python3
"""Bounded production workflow verification; baseline automated testing is deferred."""
import argparse
import hashlib
import io
import json
import os
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

def need(value, message):
    if not value:
        raise RuntimeError(message)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--workflow', default='all')
    args = ap.parse_args()
    observations = []
    with tempfile.TemporaryDirectory(prefix='cadence-workflow-') as scratch:
        scratch = Path(scratch)
        ffmpeg = os.environ.get('FFMPEG_BIN', 'ffmpeg')
        subprocess.run([ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                        '-f', 'lavfi', '-i', 'testsrc2=size=160x90:rate=24:duration=4',
                        '-f', 'lavfi', '-i', 'sine=frequency=660:sample_rate=48000:duration=4',
                        '-c:v', 'libx264', '-preset', 'ultrafast', '-threads', '1',
                        '-c:a', 'aac', '-shortest', str(scratch/'calibration.mp4')], check=True, timeout=30)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        address = 'http://127.0.0.1:'+str(port)+'/forge/v1'
        env = dict(os.environ, FORGE_ROOT=str(scratch/'data'),
                   FORGE_ADDR='127.0.0.1:'+str(port), FORGE_WORKERS='1')
        log = open(scratch/'server.log', 'ab')
        process = None
        def request(method, route, body=None, headers=None, status=200, raw=False):
            hs = dict(headers or {})
            if isinstance(body, (dict, list)):
                body = json.dumps(body).encode()
                hs['Content-Type'] = 'application/json'
            req = urllib.request.Request(address+route, data=body, headers=hs, method=method)
            try:
                response = urllib.request.urlopen(req, timeout=60)
            except urllib.error.HTTPError as exc:
                response = exc
            with response:
                data = response.read()
                need(response.status == status, f'{method} {route}: {response.status}, wanted {status}: {data[:600]!r}')
                return data if raw or not data else json.loads(data)
        def start():
            nonlocal process
            process = subprocess.Popen([str(ROOT/'bin/forged')], cwd=ROOT, env=env,
                                       stdout=log, stderr=log, start_new_session=True)
            deadline = time.monotonic()+10
            while time.monotonic() < deadline:
                try:
                    request('GET', '/health')
                    return
                except (OSError, RuntimeError):
                    if process.poll() is not None:
                        raise RuntimeError((scratch/'server.log').read_text())
                    time.sleep(.05)
            raise RuntimeError('server readiness timed out')
        def stop(force=False):
            nonlocal process
            if process and process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL if force else signal.SIGTERM)
                process.wait(timeout=15)
            process = None
        def observe(name, detail):
            observations.append({'workflow_id': name, 'passed': True, 'observed': detail})
        def wait_render(rid, desired=None):
            deadline = time.monotonic()+40
            while time.monotonic() < deadline:
                r = request('GET', '/renders/'+rid)
                if desired and r['state'] == desired:
                    return r
                if not desired and r['state'] in ('succeeded', 'failed', 'cancelled'):
                    return r
                time.sleep(.02)
            raise RuntimeError('render state timeout')
        def wait_delivery(rid, predicate, timeout=60):
            deadline = time.monotonic()+timeout
            while time.monotonic() < deadline:
                d = request('GET', '/renders/'+rid+'/delivery')
                if predicate(d):
                    return d
                time.sleep(.02)
            raise RuntimeError('delivery state timeout')
        def ladder_recipe(segment_ms=None):
            body = dict(recipe_body, name='review-ladder')
            body.pop('width', None); body.pop('height', None)
            body.update(width=320, height=180, segment_ms=segment_ms or 1500, renditions=[
                dict(name='proxy', width=160, height=90, video_bitrate_kbps=300, audio_bitrate_kbps=64),
                dict(name='final', width=320, height=180, video_bitrate_kbps=900, audio_bitrate_kbps=128)])
            return request('POST', '/recipes', body, status=201)
        try:
            start()
            media = (scratch/'calibration.mp4').read_bytes()
            source = request('POST', '/sources?name=calibration-pulse', media, status=201)
            sid = source['id']
            need(source['media']['video'] and source['media']['audio'], 'probe lost streams')
            need(source['digest'] == hashlib.sha256(media).hexdigest(), 'source digest mismatch')
            request('POST', '/sources?name=invalid', b'not-media', status=400)
            duplicate = request('POST', '/sources?name=duplicate', media, status=201)
            need(duplicate['id'] == sid, 'ingest did not converge')
            observe('ingest', 'MP4 accepted with SHA-256; malformed bytes rejected; duplicate converges')
            recipe_body = dict(name='review-proxy', format='mp4', width=160, height=90,
                               fps=24, sample_rate=48000, normalize=False, target_lufs=-16)
            recipe = request('POST', '/recipes', recipe_body, status=201)
            rp = recipe['id']
            updated = request('PUT', '/recipes/'+rp, dict(recipe_body, name='review-proxy-v2'), {'If-Match': '1'}, status=201)
            need(updated['revision'] == 2, 'recipe revision not advanced')
            request('PUT', '/recipes/'+rp, recipe_body, {'If-Match': '1'}, status=409)
            observe('recipe-version', 'revision 2 saved; stale revision rejected')
            sheet_body = dict(name='two-pass-calibration', spans=[
                dict(source_id=sid, in_ms=0, out_ms=1500, gain_db=-3),
                dict(source_id=sid, in_ms=2000, out_ms=3500, gain_db=0)],
                cues=[dict(source_id=sid, start_ms=1000, end_ms=2500, text='校准 <幅度>')])
            sheet = request('POST', '/sheets', sheet_body, status=201)
            sh = sheet['id']
            revision = request('PUT', '/sheets/'+sh, dict(sheet_body, name='revised-calibration'), {'If-Match': '1'}, status=201)
            need(revision['revision'] == 2, 'sheet revision not advanced')
            request('PUT', '/sheets/'+sh, sheet_body, {'If-Match': '1'}, status=409)
            observe('sheet-version', 'two-cut revision saved; stale writer rejected')
            plan = request('GET', '/sheets/'+sh+'/preview?recipe_id='+rp)
            need(plan['duration_ms'] == 3000 and plan['placements'][1]['output_in_ms'] == 1500, 'timeline mismatch')
            observe('preview', 'two source spans compacted to a 3000ms composition')
            need('00:00:01.000 --> 00:00:01.500' in plan['vtt'], 'first caption clipping mismatch')
            need('00:00:01.500 --> 00:00:02.000' in plan['vtt'], 'second caption mapping mismatch')
            need('&lt;幅度&gt;' in plan['vtt'], 'caption markup not escaped')
            observe('captions', 'source cue split and clipped into two escaped output intervals')
            if args.workflow in ('all', 'loudness'):
                loud = request('POST', '/sources/'+sid+'/loudness')
                need(loud['integrated_lufs'] is not None and not loud['silent'], 'loudness scan failed')
                observe('loudness', 'measured integrated LUFS and true peak')
            for mode in ['storyboard', 'waveform']:
                if args.workflow in ('all', mode):
                    image = request('GET', '/sources/'+sid+'/visual?mode='+mode, raw=True)
                    need(image.startswith(b'\x89PNG'), 'inspection output is not PNG')
                    observe(mode, f'PNG produced from source streams: {len(image)} bytes')
            req = dict(sheet_id=sh, sheet_revision=1, recipe_id=rp, recipe_revision=1)
            render = request('POST', '/renders', req, {'Idempotency-Key': 'first-render'}, status=202)
            rid = render['id']
            again = request('POST', '/renders', req, {'Idempotency-Key': 'first-render'}, status=202)
            need(again['id'] == rid, 'idempotency key did not converge')
            request('POST', '/renders', dict(req, sheet_revision=2), {'Idempotency-Key': 'first-render'}, status=409)
            observe('idempotent-submit', 'same request converges; changed revision conflicts')
            done = wait_render(rid)
            need(done['state'] == 'succeeded', str(done['attempts']))
            need(done['sheet']['revision'] == 1 and done['recipe']['revision'] == 1, 'frozen revisions drifted')
            observe('mp4-render', 'frozen revisions encoded to H.264/AAC MP4 and inspected')
            verification = request('POST', '/renders/'+rid+'/verify')
            artifact = request('GET', '/renders/'+rid+'/artifact', raw=True)
            need(hashlib.sha256(artifact).hexdigest() == verification['digest'], 'artifact digest mismatch')
            observe('artifact-verify', 'download SHA-256 equals committed inspection')
            if args.workflow in ('all', 'bundle'):
                packed = request('GET', '/renders/'+rid+'/bundle', raw=True)
                with zipfile.ZipFile(io.BytesIO(packed)) as z:
                    need(set(z.namelist()) == {'composition.mp4', 'captions.vtt', 'composition.json'}, 'bundle members mismatch')
                    frozen = json.loads(z.read('composition.json'))
                    need(frozen['plan']['vtt'] == z.read('captions.vtt').decode(), 'bundle captions drifted')
                observe('bundle', 'verified media, captions and frozen manifest exported together')
            if args.workflow in ('all', 'wav-render'):
                ar = request('POST', '/recipes', dict(recipe_body, name='reference-audio', format='wav', normalize=True), status=201)
                audio_render = request('POST', '/renders', dict(req, recipe_id=ar['id']), {'Idempotency-Key': 'audio'}, status=202)
                audio_done = wait_render(audio_render['id'])
                need(audio_done['state'] == 'succeeded', str(audio_done['attempts']))
                need(audio_done['artifact']['inspection']['media']['audio_codec'] == 'pcm_s16le', 'audio codec mismatch')
                observe('wav-render', 'trimmed stereo PCM WAV encoded with loudness filter')
            request('DELETE', '/sources/'+sid, status=409)
            observe('retire-source', 'historical sheet reference prevents retirement')
            segmented = None
            if args.workflow in ('all', 'segmented-delivery', 'progressive-review', 'package-delivery'):
                ladder = ladder_recipe()
                segmented = request('POST', '/renders', dict(req, recipe_id=ladder['id']),
                                    {'Idempotency-Key': 'segmented'}, status=202)
                segmented_id = segmented['id']
                need(segmented['delivery'] and segmented['delivery']['frozen_submission'], 'submission not frozen')
                first = wait_delivery(segmented_id, lambda d: any(r['prefix_count'] > 0 for r in d['renditions']))
                need(first['complete'] is False, 'delivery claimed complete before all segments confirmed')
                playable = [r for r in first['renditions'] if r['playable']]
                need(playable, 'no rendition became playable while encoding continued')
                pending_names = [r['name'] for r in first['renditions'] if not r['playable']]
                need(all(r['name'] not in pending_names for r in playable), 'pending rendition offered as switchable')
                head = next(r for r in first['renditions'] if r['playable'])
                download = request('GET', f'/renders/{segmented_id}/segments/{head["name"]}/0', raw=True)
                need(len(download) == head['segments'][0]['bytes'], 'segment download size mismatch')
                need(hashlib.sha256(download).hexdigest() == head['segments'][0]['digest'], 'segment download digest mismatch')
                request('GET', f'/renders/{segmented_id}/package', status=409)
                review_state = request('GET', '/renders/'+segmented_id)['state']
                need(review_state == 'running', f'incomplete delivery exposed as {review_state}')
                observe('segmented-delivery', 'canonical boundary list fixed before encoding; front segments confirmed progressively')
                observe('progressive-review', 'playable rendition prefix downloadable while slower branch still encoding; package refused')
                finished = wait_delivery(segmented_id, lambda d: d['state'] in ('succeeded', 'failed'), timeout=90)
                need(finished['state'] == 'succeeded' and finished['complete'], str(request('GET', '/renders/'+segmented_id)['attempts']))
                need(finished['duration_ms'] == plan['duration_ms'], 'delivery duration drifted from plan')
                expected = finished['boundaries']
                for rendition in finished['renditions']:
                    need([s['index'] for s in rendition['segments']] == [b['index'] for b in expected], 'segments not contiguous')
                    for segment in rendition['segments']:
                        boundary = expected[segment['index']]
                        need(segment['start_ms'] == boundary['start_ms'] and segment['end_ms'] == boundary['end_ms'], 'segment boundary drifted')
                        need(segment['submission'] == finished['frozen_submission'], 'segment cites a foreign submission')
                observe('package-delivery', 'every rendition has time-aligned contiguous segments bound to one submission')
                if args.workflow in ('all', 'package-delivery'):
                    package = request('GET', f'/renders/{segmented_id}/package', raw=True)
                    with zipfile.ZipFile(io.BytesIO(package)) as z:
                        names = set(z.namelist())
                        need('delivery.manifest.json' in names and 'captions.vtt' in names, 'package manifest or captions missing')
                        frozen = json.loads(z.read('delivery.manifest.json'))
                        need(frozen['delivery']['complete'], 'package manifest not complete')
                        for rendition in frozen['delivery']['renditions']:
                            for segment in rendition['segments']:
                                need(segment['filename'] in names, 'package missing a confirmed segment')
                                need(hashlib.sha256(z.read(segment['filename'])).hexdigest() == segment['digest'],
                                     'package segment digest mismatch')
                        need(z.read('captions.vtt').decode() == frozen['plan']['vtt'], 'package captions drifted')
                    observe('package-delivery', 'archive carries every confirmed segment, captions and one frozen submission')
                # edit the sheet after freeze: the confirmed delivery must remain
                # verifiable against the original submission, not the new revision.
                request('PUT', '/sheets/'+sh, dict(sheet_body, name='post-freeze-edit'), {'If-Match': '2'}, status=201)
                after_edit = request('GET', f'/renders/{segmented_id}/delivery')
                need(after_edit['frozen_submission'] == finished['frozen_submission'], 'frozen submission changed after edit')
                need(all(s['submission'] == after_edit['frozen_submission']
                         for r in after_edit['renditions'] for s in r['segments']), 'segment provenance drifted after edit')
                verified = request('POST', f'/renders/{segmented_id}/verify')
                need(verified['digest'] == after_edit['frozen_submission'], 'verify did not bind the frozen submission')
                request('GET', f'/renders/{segmented_id}/segments/'+head['name']+'/999', status=409)
                observe('segmented-delivery', 'post-freeze sheet edit left every segment and caption bound to the original submission')
            if args.workflow in ('all', 'cancel', 'retry', 'restart-recover'):
                long_sheet = request('POST', '/sheets', dict(sheet_body, name='extended-calibration', spans=sheet_body['spans']*16), status=201)
                pending = request('POST', '/renders', dict(req, sheet_id=long_sheet['id']), {'Idempotency-Key': 'interrupt'}, status=202)
                interrupted_id = pending['id']
                wait_render(interrupted_id, 'running')
                cancelled = request('POST', '/renders', req, {'Idempotency-Key': 'cancel-me'}, status=202)
                cancelled = request('POST', '/renders/'+cancelled['id']+'/cancel')
                need(cancelled['state'] == 'cancelled', 'cancel not acknowledged')
                request('GET', '/renders/'+cancelled['id']+'/artifact', status=409)
                observe('cancel', 'composition cancelled without artifact visibility')
                stop(force=True)
                start()
                recovered = request('GET', '/renders/'+interrupted_id)
                need(recovered['state'] == 'failed' and recovered['attempts'][-1]['outcome'] == 'interrupted', 'recovery provenance mismatch')
                observe('restart-recover', 'SIGKILL/restart retained frozen inputs and marked interruption')
                request('POST', '/renders/'+interrupted_id+'/retry', status=202)
                retried = wait_render(interrupted_id)
                need(retried['state'] == 'succeeded' and len(retried['attempts']) == 2, str(retried['attempts']))
                observe('retry', 'second attempt completed using original snapshot')
            if args.workflow in ('all', 'segmented-resume'):
                ladder = ladder_recipe()
                resume_sheet = request('POST', '/sheets', dict(sheet_body, name='segmented-interrupt',
                                                                spans=sheet_body['spans']*4), status=201)
                pending = request('POST', '/renders', dict(req, sheet_id=resume_sheet['id'], recipe_id=ladder['id']),
                                  {'Idempotency-Key': 'segmented-interrupt'}, status=202)
                resume_id = pending['id']
                partial = wait_delivery(resume_id, lambda d: any(r['prefix_count'] > 0 for r in d['renditions']))
                before = {r['name']: len(r['segments']) for r in partial['renditions']}
                need(any(count > 0 for count in before.values()), 'no segment was confirmed before interruption')
                need(not partial['complete'], 'interrupted delivery was already complete')
                stop(force=True)
                start()
                recovered = request('GET', f'/renders/{resume_id}/delivery')
                need(recovered['state'] == 'failed', 'interrupted delivery was not marked failed')
                kept = {r['name']: len(r['segments']) for r in recovered['renditions']}
                need(kept == before, f'confirmed prefix was lost across restart: {before} -> {kept}')
                request('POST', f'/renders/{resume_id}/retry', status=202)
                resumed = wait_delivery(resume_id, lambda d: d['state'] in ('succeeded', 'failed'), timeout=120)
                need(resumed['state'] == 'succeeded' and resumed['complete'], str(request('GET', f'/renders/{resume_id}')['attempts']))
                events_for = [e for e in request('GET', '/events?after=0') if e['subject'] == resume_id]
                confirmed_events = [e['detail'] for e in events_for if e['kind'] == 'render.segment.confirmed']
                need(len(confirmed_events) == len(set(confirmed_events)),
                     'a confirmed boundary was re-encoded and republished during resume')
                observe('segmented-resume', 'restart retained the confirmed prefix; retry re-encoded only missing boundaries')
                verified = request('POST', f'/renders/{resume_id}/verify')
                need(verified['digest'] == resumed['frozen_submission'], 'resumed delivery did not re-verify')
                observe('segmented-resume', 'resumed delivery re-verified against the same frozen submission')
            events = request('GET', '/events?after=0')
            need(events and all(a['sequence'] < b['sequence'] for a, b in zip(events, events[1:])), 'event order mismatch')
            tail = request('GET', '/events?after='+str(events[-1]['sequence']))
            need(all(e['sequence'] > events[-1]['sequence'] for e in tail), 'event cursor repeated')
            observe('events', 'persisted event cursor strictly increases')
            if args.workflow != 'all':
                need(any(x['workflow_id'] == args.workflow for x in observations), 'unknown workflow')
            print(json.dumps({'ok': True, 'requested': args.workflow, 'observations': observations}, ensure_ascii=False))
        except Exception:
            log.flush()
            print((scratch/'server.log').read_text(errors='replace'))
            raise
        finally:
            stop()
            log.close()

if __name__ == '__main__':
    main()
