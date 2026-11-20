# /// script
# requires-python = ">=3.10,<3.13"
# dependencies = [
#   "mlx-audio==0.6.4",
#   "silero-vad>=6,<7",
#   "soundfile>=0.12",
#   "numpy>=1.26",
# ]
# ///
"""naozhi's local meeting recogniser (docs/rfc/meeting-notes.md §3.5), run by the gateway with
`uv run --script`. Audio and text never leave this Mac.

  transcribe --input X.wav --output Y.json [--context TEXT]
      Silero VAD cuts speech into <=30 s segments (long audio otherwise drifts and invents
      text; neighbours less than 1 s apart are merged, so each segment has context);
      Qwen3-ASR-1.7B on MLX recognises each one, language pinned (see LANGUAGE). Y.json: {"engine", "segments":
      [{"start","end","text"}]}.
"""
import argparse
import json
import os
import sys

SR = 16000
MODEL = os.environ.get("NAOZHI_MEETING_ASR_MODEL", "mlx-community/Qwen3-ASR-1.7B-8bit")
# "detect" (default) picks one language per track from a sample of its segments, then
# recognises every segment pinned to it; "" leaves detection to each segment; anything else is
# pinned. Per-segment detection decides once per segment and translates a mostly-English
# segment's Chinese into English (M0, docs/rfc/meeting-notes.md §11.3: 10 minutes of
# code-switched speech, MER 43.9% auto vs 14.0% Chinese); pinning Chinese on an English meeting
# writes part of it in Chinese (AMI: WER +5–7 points). A track with Chinese in a fifth of its
# sampled segments is Chinese (English stays English when pinned to Chinese).
LANGUAGE = os.environ.get("NAOZHI_MEETING_ASR_LANGUAGE", "detect")
DETECT_SAMPLES = 16
DETECT_CHINESE_SHARE = 0.2


def speech_segments(x, max_seg=30.0, merge_gap=1.0):
    import torch
    from silero_vad import get_speech_timestamps, load_silero_vad

    torch.set_num_threads(4)
    ts = get_speech_timestamps(torch.from_numpy(x), load_silero_vad(), sampling_rate=SR, threshold=0.5,
                               min_silence_duration_ms=300, speech_pad_ms=150,
                               max_speech_duration_s=max_seg - 0.5)
    merged = []
    for t in ts:
        s, e = t["start"] / SR, t["end"] / SR
        if merged and s - merged[-1][1] <= merge_gap and e - merged[-1][0] <= max_seg:
            merged[-1] = (merged[-1][0], e)
        else:
            merged.append((s, e))
    return merged


def detect_language(model, x, segs):
    import numpy as np

    pick = np.linspace(0, len(segs) - 1, min(DETECT_SAMPLES, len(segs))).round().astype(int)
    langs = []
    for i in sorted(set(pick.tolist())):
        s, e = segs[i]
        clip = x[int(s * SR):int(e * SR)]
        if len(clip) < SR // 2:
            continue
        r = model.generate(clip, language=None, max_tokens=8)
        if r.segments:
            langs.append(r.segments[0].get("language"))
    if not langs:
        return "Chinese"
    if sum(1 for l in langs if l == "Chinese") >= DETECT_CHINESE_SHARE * len(langs):
        return "Chinese"
    top = max(set(langs), key=langs.count)
    return top or ""


def transcribe(args):
    import numpy as np
    import soundfile as sf

    x, sr = sf.read(args.input, dtype="float32", always_2d=True)
    if sr != SR:
        sys.exit(f"{args.input}: {sr} Hz, want 16000")
    x = x.mean(axis=1)
    segs = speech_segments(x)
    out = []
    if segs:
        from mlx_audio.stt import load

        model = load(MODEL)
        language = LANGUAGE
        if language == "detect":
            language = detect_language(model, x, segs)
        for s, e in segs:
            clip = x[int(s * SR):int(e * SR)]
            if len(clip) < SR // 10:
                clip = np.pad(clip, (0, SR // 10 - len(clip)))
            r = model.generate(clip, language=language or None, system_prompt=args.context or None, max_tokens=512)
            text = " ".join(r.text.split())
            if text:
                out.append({"start": round(s, 2), "end": round(e, 2), "text": text})
    tmp = args.output + ".tmp"
    with open(tmp, "w") as f:
        json.dump({"engine": "qwen3-asr-1.7b-mlx", "model": MODEL, "language": language if segs else "", "segments": out}, f, ensure_ascii=False)
    os.replace(tmp, args.output)


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    t = sub.add_parser("transcribe")
    t.add_argument("--input", required=True)
    t.add_argument("--output", required=True)
    t.add_argument("--context", default="")
    args = ap.parse_args()
    if args.cmd == "transcribe":
        transcribe(args)


if __name__ == "__main__":
    main()
