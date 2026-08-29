#Requires AutoHotkey v2.0
#SingleInstance Force
;
; OPTIONAL FALLBACK. You do not need this file.
;
; The daemon captures the media keys itself with a low-level keyboard hook, so
; there is nothing for AutoHotkey to do. Use this script only if you set
; "hotkeys": false in config.json (or run the daemon with -no-hotkeys) and
; would rather drive it from AHK.
;
; Every call is fire-and-forget: a knob spin must never wait on a response.

Port := 8888
Base := "http://127.0.0.1:" Port
DoublePressMS := 250

; Keep references to in-flight requests alive; releasing the COM object early
; can abort the async request before it is sent.
Pending := []

Post(path) {
    global Pending
    try {
        req := ComObject("WinHttp.WinHttpRequest.5.1")
        req.SetTimeouts(300, 300, 300, 300)
        req.Open("POST", Base . path, true)   ; true = async
        req.Send()
        Pending.Push(req)
        if (Pending.Length > 20)
            Pending.RemoveAt(1)
    } catch {
        ; Daemon down: stay silent rather than freezing the keyboard.
    }
}

; --- knob rotation -----------------------------------------------------------
; Buffering keeps fast spins from being dropped. These directives are
; positional: they apply to the hotkeys defined below them.
#MaxThreadsPerHotkey 5
#MaxThreadsBuffer True

Volume_Up:: Post("/volume/up")
Volume_Down:: Post("/volume/down")

; --- knob press --------------------------------------------------------------
; Buffering must be OFF here, or a second press gets queued and replays after
; the double-press handler finishes (previous, then an immediate next).
#MaxThreadsPerHotkey 1
#MaxThreadsBuffer False

Volume_Mute:: {
    KeyWait("Volume_Mute")
    if KeyWait("Volume_Mute", "D T" . (DoublePressMS / 1000)) {
        KeyWait("Volume_Mute")
        Post("/previous")
    } else {
        Post("/next")
    }
}
