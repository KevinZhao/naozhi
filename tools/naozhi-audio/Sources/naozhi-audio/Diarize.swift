import FluidAudio
import Foundation

// diarize IN.wav OUT.json [-models DIR]: speaker turns of one track (§3.5), FluidAudio's offline
// pipeline (CoreML port of pyannote community-1: segmentation, WeSpeaker embeddings, PLDA, VBx)
// with community-1's own post-processing — keep short segments, 10% window step. M0 measured it
// 1–3 DER points behind pyannote at a tenth of the time. The models (~34 MB) are fetched into
// DIR on first use.
func runDiarize(_ args: [String]) -> Never {
    let pos = args.filter { !$0.hasPrefix("-") }
    guard pos.count >= 2 else { fail(64, "diarize IN.wav OUT.json [-models DIR]") }
    let input = URL(fileURLWithPath: pos[0]), output = pos[1]
    let modelsDir = flagValues(args, "models").last.map { URL(fileURLWithPath: $0, isDirectory: true) }
    let sem = DispatchSemaphore(value: 0)
    var code: Int32 = 0
    Task {
        do {
            let cfg = OfflineDiarizerConfig(segmentationStepRatio: 0.1, minSegmentDuration: 0, minGapDuration: 0)
            let models = try await OfflineDiarizerModels.load(from: modelsDir)
            let manager = OfflineDiarizerManager(config: cfg)
            manager.initialize(models: models)
            let src = try AudioSourceFactory().makeDiskBackedSource(from: input, targetSampleRate: cfg.segmentation.sampleRate)
            defer { src.source.cleanup() }
            let result = try await manager.process(audioSource: src.source, audioLoadingSeconds: src.loadDuration) { _, _ in }
            let turns: [[String: Any]] = result.segments.map {
                ["start": Double($0.startTimeSeconds), "end": Double($0.endTimeSeconds), "speaker": $0.speakerId]
            }
            let data = try JSONSerialization.data(withJSONObject: ["engine": "fluidaudio-offline-0.18.2", "turns": turns])
            let tmp = output + ".tmp"
            try data.write(to: URL(fileURLWithPath: tmp))
            _ = try FileManager.default.replaceItemAt(URL(fileURLWithPath: output), withItemAt: URL(fileURLWithPath: tmp))
            emit(["event": "diarized", "path": output, "turns": turns.count, "speakers": Set(result.segments.map { $0.speakerId }).count])
        } catch {
            emit(["event": "error", "error": "diarize: \(error)"])
            code = 70
        }
        sem.signal()
    }
    sem.wait()
    exit(code)
}
