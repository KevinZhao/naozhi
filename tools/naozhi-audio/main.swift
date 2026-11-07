// naozhi-audio: the macOS audio helper behind meeting recording (docs/rfc/meeting-notes.md §3.1).
//
// The gateway runs it as a subprocess; every subcommand prints JSON (one object per line).
//
//   status                       microphone / system-audio authorisation, never prompts
//   request                      ask for both authorisations (shows the system prompts once)
//   watch [-interval 2]          a line whenever the set of processes using audio changes
//   record -out DIR -bundle B... [-segment 30] [-start-index N]
//                                mic + a process tap of the bundles, one private aggregate
//                                device, 16 kHz mono WAV segments in DIR/mic and DIR/remote
//   encode IN.wav OUT.m4a        AAC copy for retention
//
// Authorisation is attributed to the "responsible process". Spawned by the gateway that is the
// gateway itself (and every agent session under it), so the subcommands that touch TCC re-spawn
// this binary with responsibility disclaimed: the grant then belongs to this signed helper alone.
import AVFoundation
import AudioToolbox
import CoreAudio
import Darwin
import Foundation

setvbuf(stdout, nil, _IOLBF, 0)

let encoder = JSONSerialization.self
func emit(_ obj: [String: Any]) {
    guard let data = try? encoder.data(withJSONObject: obj, options: [.sortedKeys]) else { return }
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write("\n".data(using: .utf8)!)
}

func fail(_ code: Int32, _ msg: String) -> Never {
    emit(["event": "error", "error": msg])
    exit(code)
}

// MARK: - Core Audio property helpers

func prop<T>(_ obj: AudioObjectID, _ sel: AudioObjectPropertySelector, scope: AudioObjectPropertyScope = kAudioObjectPropertyScopeGlobal, _ zero: T) -> (T, OSStatus) {
    var addr = AudioObjectPropertyAddress(mSelector: sel, mScope: scope, mElement: kAudioObjectPropertyElementMain)
    var size = UInt32(MemoryLayout<T>.size)
    var v = zero
    let st = AudioObjectGetPropertyData(obj, &addr, 0, nil, &size, &v)
    return (v, st)
}

func propObjects(_ obj: AudioObjectID, _ sel: AudioObjectPropertySelector) -> [AudioObjectID] {
    var addr = AudioObjectPropertyAddress(mSelector: sel, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
    var size: UInt32 = 0
    guard AudioObjectGetPropertyDataSize(obj, &addr, 0, nil, &size) == noErr else { return [] }
    var ids = [AudioObjectID](repeating: 0, count: Int(size) / MemoryLayout<AudioObjectID>.size)
    guard AudioObjectGetPropertyData(obj, &addr, 0, nil, &size, &ids) == noErr else { return [] }
    return ids
}

func propString(_ obj: AudioObjectID, _ sel: AudioObjectPropertySelector) -> String? {
    var addr = AudioObjectPropertyAddress(mSelector: sel, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
    var size = UInt32(MemoryLayout<Unmanaged<CFString>?>.size)
    var s: Unmanaged<CFString>?
    guard AudioObjectGetPropertyData(obj, &addr, 0, nil, &size, &s) == noErr else { return nil }
    return s?.takeRetainedValue() as String?
}

func inputStreamChannels(_ dev: AudioObjectID) -> [Int] {
    var addr = AudioObjectPropertyAddress(mSelector: kAudioDevicePropertyStreamConfiguration, mScope: kAudioObjectPropertyScopeInput, mElement: kAudioObjectPropertyElementMain)
    var size: UInt32 = 0
    guard AudioObjectGetPropertyDataSize(dev, &addr, 0, nil, &size) == noErr, size > 0 else { return [] }
    let raw = UnsafeMutableRawPointer.allocate(byteCount: Int(size), alignment: 16)
    defer { raw.deallocate() }
    guard AudioObjectGetPropertyData(dev, &addr, 0, nil, &size, raw) == noErr else { return [] }
    return UnsafeMutableAudioBufferListPointer(raw.assumingMemoryBound(to: AudioBufferList.self)).map { Int($0.mNumberChannels) }
}

func procPath(_ pid: pid_t) -> String {
    var buf = [CChar](repeating: 0, count: 4096)
    return proc_pidpath(pid, &buf, UInt32(buf.count)) > 0 ? String(cString: buf) : ""
}

func parentPID(_ pid: pid_t) -> pid_t {
    var info = proc_bsdinfo()
    let n = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, Int32(MemoryLayout<proc_bsdinfo>.size))
    return n > 0 ? pid_t(info.pbi_ppid) : -1
}

struct AudioProc {
    let obj: AudioObjectID
    let pid: pid_t
    let bundle: String
    let input: Bool
    let output: Bool
    var json: [String: Any] { ["pid": Int(pid), "ppid": Int(parentPID(pid)), "bundle": bundle, "path": procPath(pid), "input": input, "output": output] }
}

// Reading the process list and the running-input flags needs no authorisation.
func audioProcesses() -> [AudioProc] {
    propObjects(AudioObjectID(kAudioObjectSystemObject), kAudioHardwarePropertyProcessObjectList).map { id in
        let (pid, _) = prop(id, kAudioProcessPropertyPID, pid_t(-1))
        let (ri, _) = prop(id, kAudioProcessPropertyIsRunningInput, UInt32(0))
        let (ro, _) = prop(id, kAudioProcessPropertyIsRunningOutput, UInt32(0))
        return AudioProc(obj: id, pid: pid, bundle: propString(id, kAudioProcessPropertyBundleID) ?? "", input: ri != 0, output: ro != 0)
    }
}

// MARK: - TCC

// TCC.framework's preflight/request are SPI; there is no public preflight for system-audio
// capture, and without one a missing grant is indistinguishable from silence.
let tcc = dlopen("/System/Library/PrivateFrameworks/TCC.framework/Versions/A/TCC", RTLD_NOW)
typealias PreflightFn = @convention(c) (CFString, CFDictionary?) -> Int
typealias RequestFn = @convention(c) (CFString, CFDictionary?, @escaping (Bool) -> Void) -> Void

func audioCaptureStatus() -> String {
    guard let tcc, let sym = dlsym(tcc, "TCCAccessPreflight") else { return "unknown" }
    switch unsafeBitCast(sym, to: PreflightFn.self)("kTCCServiceAudioCapture" as CFString, nil) {
    case 0: return "authorized"
    case 1: return "denied"
    default: return "not_determined"
    }
}

func requestAudioCapture() -> Bool {
    guard let tcc, let sym = dlsym(tcc, "TCCAccessRequest") else { return false }
    let sem = DispatchSemaphore(value: 0)
    var ok = false
    unsafeBitCast(sym, to: RequestFn.self)("kTCCServiceAudioCapture" as CFString, nil) { g in ok = g; sem.signal() }
    _ = sem.wait(timeout: .now() + 300)
    return ok
}

func micStatus() -> String {
    switch AVCaptureDevice.authorizationStatus(for: .audio) {
    case .authorized: return "authorized"
    case .denied, .restricted: return "denied"
    case .notDetermined: return "not_determined"
    @unknown default: return "unknown"
    }
}

func statusJSON() -> [String: Any] {
    ["event": "status", "microphone": micStatus(), "audio_capture": audioCaptureStatus()]
}

// MARK: - responsibility disclaim

let disclaimedEnv = "NAOZHI_AUDIO_DISCLAIMED"

// Re-spawn self as its own responsible process (private libsystem SPI, also used by Terminal and
// LLDB), forward termination signals and pass the exit status through. stdin/stdout are inherited,
// so the child still sees the gateway's pipe close.
func respawnDisclaimed() -> Never {
    typealias DisclaimFn = @convention(c) (UnsafeMutablePointer<posix_spawnattr_t?>, Int32) -> Int32
    var attr: posix_spawnattr_t?
    posix_spawnattr_init(&attr)
    if let sym = dlsym(UnsafeMutableRawPointer(bitPattern: -2), "responsibility_spawnattrs_setdisclaim") {
        _ = unsafeBitCast(sym, to: DisclaimFn.self)(&attr, 1)
    }
    setenv(disclaimedEnv, "1", 1)
    let exe = procPath(getpid())
    let argv: [UnsafeMutablePointer<CChar>?] = CommandLine.arguments.enumerated().map { strdup($0.offset == 0 ? exe : $0.element) } + [nil]
    var child: pid_t = 0
    let rc = posix_spawn(&child, exe, nil, &attr, argv, environ)
    if rc != 0 { fail(70, "posix_spawn: \(rc)") }
    for sig in [SIGTERM, SIGINT, SIGHUP] {
        signal(sig, SIG_IGN)
        let src = DispatchSource.makeSignalSource(signal: sig, queue: .main)
        src.setEventHandler { kill(child, sig) }
        src.resume()
        signalSources.append(src)
    }
    DispatchQueue.global().async {
        var status: Int32 = 0
        while waitpid(child, &status, 0) < 0 && errno == EINTR {}
        let code: Int32 = (status & 0x7f) == 0 ? (status >> 8) & 0xff : 128 + (status & 0x7f)
        exit(code)
    }
    dispatchMain()
}

var signalSources: [DispatchSourceSignal] = []

// MARK: - recording

let targetRate = 16000.0

// One track's segment writer: float32 at the device rate in, 16 kHz mono Int16 WAV out.
final class SegmentTrack {
    let dir: String
    let inFmt: AVAudioFormat
    let outFmt: AVAudioFormat
    let converter: AVAudioConverter
    var file: AVAudioFile?
    var partPath = ""
    var peak: Float = 0
    var sumSq: Double = 0
    var count = 0

    init(dir: String, inputRate: Double) throws {
        self.dir = dir
        inFmt = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: inputRate, channels: 1, interleaved: false)!
        outFmt = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: targetRate, channels: 1, interleaved: true)!
        guard let c = AVAudioConverter(from: inFmt, to: outFmt) else { throw NSError(domain: "naozhi-audio", code: 1) }
        converter = c
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    }

    func open(index: Int) throws {
        partPath = String(format: "%@/%06d.wav.part", dir, index)
        let settings: [String: Any] = [AVFormatIDKey: kAudioFormatLinearPCM, AVSampleRateKey: targetRate, AVNumberOfChannelsKey: 1,
                                       AVLinearPCMBitDepthKey: 16, AVLinearPCMIsFloatKey: false, AVLinearPCMIsBigEndianKey: false]
        file = try AVAudioFile(forWriting: URL(fileURLWithPath: partPath), settings: settings, commonFormat: .pcmFormatInt16, interleaved: true)
        peak = 0; sumSq = 0; count = 0
    }

    // samples are mono float at the device rate
    func write(_ samples: [Float]) {
        guard !samples.isEmpty, let file else { return }
        for s in samples { let a = abs(s); if a > peak { peak = a }; sumSq += Double(s * s) }
        count += samples.count
        let inBuf = AVAudioPCMBuffer(pcmFormat: inFmt, frameCapacity: AVAudioFrameCount(samples.count))!
        inBuf.frameLength = AVAudioFrameCount(samples.count)
        samples.withUnsafeBufferPointer { inBuf.floatChannelData![0].update(from: $0.baseAddress!, count: samples.count) }
        let cap = AVAudioFrameCount(Double(samples.count) * targetRate / inFmt.sampleRate) + 64
        let outBuf = AVAudioPCMBuffer(pcmFormat: outFmt, frameCapacity: cap)!
        var fed = false
        var err: NSError?
        converter.convert(to: outBuf, error: &err) { _, status in
            if fed { status.pointee = .noDataNow; return nil }
            fed = true
            status.pointee = .haveData
            return inBuf
        }
        if outBuf.frameLength > 0 { try? file.write(from: outBuf) }
    }

    // Closes the segment and renames it so the gateway only ever reads complete files.
    func close() -> (String, Float, Double) {
        file = nil
        let done = String(partPath.dropLast(".part".count))
        try? FileManager.default.moveItem(atPath: partPath, toPath: done)
        let rms = count > 0 ? (sumSq / Double(count)).squareRoot() : 0
        return (done, peak, rms)
    }
}

final class Recorder {
    let out: String
    let bundles: [String]
    let segmentFrames: Int
    var index: Int
    var tapID = AudioObjectID(0)
    var agg = AudioObjectID(0)
    var procID: AudioDeviceIOProcID?
    var mic: SegmentTrack!
    var remote: SegmentTrack!
    var micStreams = 0
    var rate = 48000.0
    // IOProc → writer hand-off; the IO thread only copies samples.
    var lock = os_unfair_lock()
    var pendingMic: [Float] = []
    var pendingRemote: [Float] = []
    var framesInSegment = 0
    var segmentStart = Date()
    var startedAt = Date()
    let writer = DispatchQueue(label: "naozhi-audio.writer")
    var stopping = false

    init(out: String, bundles: [String], segmentSeconds: Int, startIndex: Int) {
        self.out = out
        self.bundles = bundles
        self.index = startIndex
        segmentFrames = segmentSeconds
    }

    func matchingProcesses() -> [AudioProc] {
        audioProcesses().filter { p in bundles.contains { !$0.isEmpty && p.bundle.hasPrefix($0) } }
    }

    func start() {
        let (micDev, _) = prop(AudioObjectID(kAudioObjectSystemObject), kAudioHardwarePropertyDefaultInputDevice, AudioObjectID(0))
        guard micDev != 0, let micUID = propString(micDev, kAudioDevicePropertyDeviceUID) else { fail(66, "no default input device") }
        let procs = matchingProcesses()
        if procs.isEmpty { fail(66, "no audio process matches \(bundles)") }
        let tapDesc: CATapDescription
        if #available(macOS 26.0, *) {
            // Bundle IDs (with restore) keep helper processes the meeting app starts later in the tap.
            tapDesc = CATapDescription(monoMixdownOfProcesses: procs.map { $0.obj })
            tapDesc.bundleIDs = Array(Set(procs.map { $0.bundle }))
            tapDesc.isProcessRestoreEnabled = true
        } else {
            tapDesc = CATapDescription(monoMixdownOfProcesses: procs.map { $0.obj })
        }
        tapDesc.uuid = UUID()
        tapDesc.muteBehavior = .unmuted
        tapDesc.isPrivate = true
        tapDesc.name = "naozhi-audio"
        var st = AudioHardwareCreateProcessTap(tapDesc, &tapID)
        if st != noErr { fail(69, "create process tap: \(st)") }
        let desc: [String: Any] = [
            kAudioAggregateDeviceNameKey: "naozhi-audio",
            kAudioAggregateDeviceUIDKey: UUID().uuidString,
            kAudioAggregateDeviceMainSubDeviceKey: micUID,
            kAudioAggregateDeviceIsPrivateKey: true,
            kAudioAggregateDeviceIsStackedKey: false,
            kAudioAggregateDeviceTapAutoStartKey: true,
            kAudioAggregateDeviceSubDeviceListKey: [[kAudioSubDeviceUIDKey: micUID]],
            kAudioAggregateDeviceTapListKey: [[kAudioSubTapDriftCompensationKey: true, kAudioSubTapUIDKey: tapDesc.uuid.uuidString]],
        ]
        st = AudioHardwareCreateAggregateDevice(desc as CFDictionary, &agg)
        if st != noErr { cleanup(); fail(69, "create aggregate device: \(st)") }
        rate = prop(agg, kAudioDevicePropertyNominalSampleRate, Float64(48000)).0
        micStreams = inputStreamChannels(micDev).count
        let layout = inputStreamChannels(agg)
        if layout.count <= micStreams { cleanup(); fail(69, "aggregate device has no tap stream (layout \(layout))") }
        do {
            mic = try SegmentTrack(dir: out + "/mic", inputRate: rate)
            remote = try SegmentTrack(dir: out + "/remote", inputRate: rate)
            try mic.open(index: index)
            try remote.open(index: index)
        } catch { cleanup(); fail(73, "open segment: \(error)") }
        let ms = micStreams
        st = AudioDeviceCreateIOProcIDWithBlock(&procID, agg, nil) { [unowned self] _, inData, _, _, _ in
            let abl = UnsafeMutableAudioBufferListPointer(UnsafeMutablePointer(mutating: inData))
            guard abl.count > ms else { return }
            let m = Recorder.firstChannel(abl[0])
            let r = Recorder.mixdown(abl, from: ms)
            os_unfair_lock_lock(&self.lock)
            self.pendingMic.append(contentsOf: m)
            self.pendingRemote.append(contentsOf: r)
            os_unfair_lock_unlock(&self.lock)
        }
        if st != noErr { cleanup(); fail(69, "create IOProc: \(st)") }
        st = AudioDeviceStart(agg, procID)
        if st != noErr { cleanup(); fail(69, "start device: \(st)") }
        startedAt = Date()
        segmentStart = startedAt
        emit(["event": "started", "rate": rate, "mic_device": micUID, "streams": layout, "index": index,
              "processes": procs.map { $0.json }])
        watchDefaultInput()
        let timer = DispatchSource.makeTimerSource(queue: writer)
        timer.schedule(deadline: .now() + 0.25, repeating: 0.25)
        timer.setEventHandler { [unowned self] in self.drain() }
        timer.resume()
        timers.append(timer)
    }

    static func firstChannel(_ b: AudioBuffer) -> [Float] {
        let n = Int(max(b.mNumberChannels, 1))
        guard let p = b.mData?.assumingMemoryBound(to: Float.self) else { return [] }
        let frames = Int(b.mDataByteSize) / (4 * n)
        return (0..<frames).map { p[$0 * n] }
    }

    static func mixdown(_ abl: UnsafeMutableAudioBufferListPointer, from: Int) -> [Float] {
        var out: [Float] = []
        var chans = 0
        for i in from..<abl.count {
            let b = abl[i]
            let n = Int(max(b.mNumberChannels, 1))
            guard let p = b.mData?.assumingMemoryBound(to: Float.self) else { continue }
            let frames = Int(b.mDataByteSize) / (4 * n)
            if out.isEmpty { out = [Float](repeating: 0, count: frames) }
            for f in 0..<min(frames, out.count) { for c in 0..<n { out[f] += p[f * n + c] } }
            chans += n
        }
        if chans > 1 { let k = 1 / Float(chans); for i in out.indices { out[i] *= k } }
        return out
    }

    // Runs on the writer queue: both tracks advance by the same frame count, so segment k of mic
    // and remote cover the same interval of the aggregate device's clock.
    func drain() {
        os_unfair_lock_lock(&lock)
        let n = min(pendingMic.count, pendingRemote.count)
        let m = Array(pendingMic.prefix(n)), r = Array(pendingRemote.prefix(n))
        pendingMic.removeFirst(n)
        pendingRemote.removeFirst(n)
        os_unfair_lock_unlock(&lock)
        var off = 0
        let perSegment = Int(rate) * segmentFrames
        while off < n {
            let take = min(n - off, perSegment - framesInSegment)
            mic.write(Array(m[off..<off + take]))
            remote.write(Array(r[off..<off + take]))
            framesInSegment += take
            off += take
            if framesInSegment >= perSegment { rotate(final: false) }
        }
    }

    func rotate(final: Bool) {
        let (mp, mpeak, mrms) = mic.close()
        let (rp, rpeak, rrms) = remote.close()
        let secs = Double(framesInSegment) / rate
        emit(["event": "segment", "index": index, "seconds": secs,
              "offset": segmentStart.timeIntervalSince(startedAt),
              "mic": ["path": mp, "peak": mpeak, "rms": mrms],
              "remote": ["path": rp, "peak": rpeak, "rms": rrms]])
        framesInSegment = 0
        index += 1
        segmentStart = Date()
        if !final {
            do { try mic.open(index: index); try remote.open(index: index) } catch { fail(73, "open segment: \(error)") }
        }
    }

    // A changed default input (headset plugged in, AirPods) leaves the aggregate on the old mic;
    // stop cleanly and let the gateway start the next part on the new device.
    func watchDefaultInput() {
        var addr = AudioObjectPropertyAddress(mSelector: kAudioHardwarePropertyDefaultInputDevice, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
        AudioObjectAddPropertyListenerBlock(AudioObjectID(kAudioObjectSystemObject), &addr, writer) { [unowned self] _, _ in
            self.finish(code: 75, reason: "input_device_changed")
        }
    }

    func finish(code: Int32, reason: String) {
        if stopping { return }
        stopping = true
        if let procID { AudioDeviceStop(agg, procID) }
        drain()
        if framesInSegment > 0 { rotate(final: true) } else { _ = mic.close(); _ = remote.close(); removeEmptyParts() }
        cleanup()
        emit(["event": "stopped", "reason": reason, "next_index": index])
        exit(code)
    }

    func removeEmptyParts() {
        for d in [out + "/mic", out + "/remote"] {
            let p = String(format: "%@/%06d.wav", d, index)
            try? FileManager.default.removeItem(atPath: p)
        }
    }

    func cleanup() {
        if let procID, agg != 0 { AudioDeviceDestroyIOProcID(agg, procID) }
        procID = nil
        if agg != 0 { AudioHardwareDestroyAggregateDevice(agg); agg = 0 }
        if tapID != 0 { AudioHardwareDestroyProcessTap(tapID); tapID = 0 }
    }
}

var timers: [DispatchSourceTimer] = []

func flagValues(_ args: [String], _ name: String) -> [String] {
    var out: [String] = []
    var i = 0
    while i < args.count {
        if args[i] == "-" + name || args[i] == "--" + name, i + 1 < args.count { out.append(args[i + 1]); i += 2 } else { i += 1 }
    }
    return out
}

func runRecord(_ args: [String]) -> Never {
    guard let out = flagValues(args, "out").last else { fail(64, "record: -out required") }
    let bundles = flagValues(args, "bundle")
    if bundles.isEmpty { fail(64, "record: at least one -bundle required") }
    let seg = Int(flagValues(args, "segment").last ?? "30") ?? 30
    let start = Int(flagValues(args, "start-index").last ?? "1") ?? 1
    if audioCaptureStatus() == "denied" { fail(77, "system audio recording not authorised") }
    if micStatus() == "denied" { fail(77, "microphone not authorised") }
    let rec = Recorder(out: out, bundles: bundles, segmentSeconds: max(seg, 1), startIndex: max(start, 1))
    rec.start()
    for sig in [SIGTERM, SIGINT, SIGHUP] {
        signal(sig, SIG_IGN)
        let src = DispatchSource.makeSignalSource(signal: sig, queue: rec.writer)
        src.setEventHandler { rec.finish(code: 0, reason: "signal") }
        src.resume()
        signalSources.append(src)
    }
    // stdin closing means the gateway is gone; do not keep the microphone open without it.
    DispatchQueue.global().async {
        while FileHandle.standardInput.availableData.count > 0 {}
        rec.writer.async { rec.finish(code: 0, reason: "stdin_closed") }
    }
    dispatchMain()
}

func runWatch(_ args: [String]) -> Never {
    let interval = Double(flagValues(args, "interval").last ?? "2") ?? 2
    var last = ""
    var lastEmit = Date.distantPast
    let timer = DispatchSource.makeTimerSource(queue: .main)
    timer.schedule(deadline: .now(), repeating: max(interval, 0.5))
    timer.setEventHandler {
        let active = audioProcesses().filter { $0.input || $0.output }.sorted { $0.pid < $1.pid }
        let key = active.map { "\($0.pid):\($0.input):\($0.output)" }.joined(separator: ",")
        if key != last || Date().timeIntervalSince(lastEmit) > 60 {
            last = key
            lastEmit = Date()
            emit(["event": "procs", "procs": active.map { $0.json }])
        }
    }
    timer.resume()
    timers.append(timer)
    DispatchQueue.global().async {
        while FileHandle.standardInput.availableData.count > 0 {}
        exit(0)
    }
    dispatchMain()
}

func runEncode(_ args: [String]) -> Never {
    guard args.count >= 2 else { fail(64, "encode IN.wav OUT.m4a") }
    do {
        let input = try AVAudioFile(forReading: URL(fileURLWithPath: args[0]))
        let settings: [String: Any] = [AVFormatIDKey: kAudioFormatMPEG4AAC, AVSampleRateKey: input.fileFormat.sampleRate,
                                       AVNumberOfChannelsKey: 1, AVEncoderBitRateKey: 32000]
        let output = try AVAudioFile(forWriting: URL(fileURLWithPath: args[1]), settings: settings)
        let buf = AVAudioPCMBuffer(pcmFormat: input.processingFormat, frameCapacity: 16384)!
        while input.framePosition < input.length {
            try input.read(into: buf)
            if buf.frameLength == 0 { break }
            try output.write(from: buf)
        }
    } catch { fail(73, "encode: \(error)") }
    emit(["event": "encoded", "path": args[1]])
    exit(0)
}

// MARK: - main

let argv = Array(CommandLine.arguments.dropFirst())
guard let cmd = argv.first else { fail(64, "usage: naozhi-audio status|request|watch|record|encode") }
let rest = Array(argv.dropFirst())
let needsOwnIdentity = ["status", "request", "record"].contains(cmd)
if needsOwnIdentity && getenv(disclaimedEnv) == nil { respawnDisclaimed() }

switch cmd {
case "status":
    emit(statusJSON())
case "request":
    let sem = DispatchSemaphore(value: 0)
    AVCaptureDevice.requestAccess(for: .audio) { _ in sem.signal() }
    _ = sem.wait(timeout: .now() + 300)
    if audioCaptureStatus() != "authorized" { _ = requestAudioCapture() }
    emit(statusJSON())
case "watch":
    runWatch(rest)
case "record":
    runRecord(rest)
case "encode":
    runEncode(rest)
case "version":
    emit(["event": "version", "version": Bundle.main.infoDictionary?["CFBundleShortVersionString"] ?? "dev"])
default:
    fail(64, "unknown subcommand \(cmd)")
}
