package main

import (
    "encoding/json"
    "fmt"
    "log"
    "net/http"
)

func main() {
    mux := http.NewServeMux()
    mux.HandleFunc("/", handlePage)
    mux.HandleFunc("/api/match", handleMatch)

    addr := ":8080"
    log.Printf("Listening on %s", addr)
    if err := http.ListenAndServe(addr, mux); err != nil {
        log.Fatal(err)
    }
}

func handlePage(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    fmt.Fprint(w, pageHTML)
}

func handleMatch(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    defer r.Body.Close()
    var payload struct {
        Transcript string `json:"transcript"`
    }

    if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }

    log.Printf("Transcript received: %q", payload.Transcript)
    w.WriteHeader(http.StatusOK)
}

const pageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Voice Match Recorder</title>
  <style>
    body { font-family: system-ui, sans-serif; margin: 0; padding: 24px; background: #f6f8fb; color: #1f2933; }
    h1 { margin-top: 0; }
    button { padding: 12px 20px; border: none; border-radius: 999px; background: #2563eb; color: white; font-size: 1rem; cursor: pointer; transition: background 0.2s ease; }
    button[disabled] { background: #94a3b8; cursor: not-allowed; }
    button.listening { background: #dc2626; }
    #status { margin-top: 12px; color: #475569; }
    #transcript { margin-top: 24px; padding: 16px; min-height: 120px; border-radius: 12px; background: white; box-shadow: 0 10px 30px rgba(15, 23, 42, 0.08); line-height: 1.5; white-space: pre-wrap; }
    #error { margin-top: 16px; color: #dc2626; }
  </style>
</head>
<body>
  <h1>Voice Match Recorder</h1>
  <p>Tap the microphone, describe the match, and we'll transcribe it in real time.</p>
  <button id="mic" type="button">🎙️ Start Listening</button>
  <div id="status">Speech recognition idle.</div>
  <div id="transcript"></div>
  <div id="error"></div>
  <script>
    const micButton = document.getElementById('mic');
    const statusEl = document.getElementById('status');
    const transcriptEl = document.getElementById('transcript');
    const errorEl = document.getElementById('error');

    let recognition;
    let listening = false;
    let finalTranscript = '';

    function makeRecognition() {
      const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
      if (!SpeechRecognition) {
        errorEl.textContent = 'SpeechRecognition is not supported in this browser.';
        micButton.disabled = true;
        return null;
      }
      const recog = new SpeechRecognition();
      recog.lang = 'en-US';
      recog.interimResults = true;
      recog.continuous = false;
      return recog;
    }

    recognition = makeRecognition();

    function setListening(active) {
      listening = active;
      micButton.textContent = active ? '🛑 Stop Listening' : '🎙️ Start Listening';
      micButton.classList.toggle('listening', active);
      statusEl.textContent = active ? 'Listening…' : 'Speech recognition idle.';
    }

    function resetTranscript() {
      finalTranscript = '';
      transcriptEl.textContent = '';
    }

    if (recognition) {
      recognition.onresult = (event) => {
        let interim = '';
        for (let i = event.resultIndex; i < event.results.length; i++) {
          const result = event.results[i];
          if (result.isFinal) {
            finalTranscript += result[0].transcript.trim() + ' ';
          } else {
            interim += result[0].transcript;
          }
        }
        const combined = (finalTranscript + interim).trim();
        transcriptEl.textContent = combined;
      };

      recognition.onerror = (event) => {
        errorEl.textContent = 'Speech recognition error: ' + event.error;
        setListening(false);
      };

      recognition.onend = () => {
        setListening(false);
        const payload = finalTranscript.trim();
        if (!payload) {
          return;
        }
        fetch('/api/match', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ transcript: payload })
        }).then(() => {
          statusEl.textContent = 'Transcript sent to server.';
          finalTranscript = '';
        }).catch(err => {
          errorEl.textContent = 'Failed to send transcript: ' + err;
        });
      };
    }

    micButton.addEventListener('click', () => {
      errorEl.textContent = '';
      if (!recognition) return;

      if (!listening) {
        resetTranscript();
        try {
          recognition.start();
          setListening(true);
        } catch (err) {
          errorEl.textContent = 'Cannot start recognition: ' + err.message;
        }
      } else {
        recognition.stop();
      }
    });
  </script>
</body>
</html>`
