class WebrtcApp {
    constructor() {
        this.entryForm = document.getElementById('container__entry__form');
        this.entryContainer = document.getElementById('entry-form-container');
        this.sessionContainer = document.getElementById('session-container');
        this.backBtn = document.getElementById('back-btn');
        this.userInfo = document.getElementById('user-info');
        this.logs = document.getElementById('logs');

        this.roomID = null;
        this.peerID = null;
        this.lang = null;

        this.initEventListeners();
    }

    initEventListeners() {
        this.entryForm.addEventListener('submit', async (e) => {
            this.showAlert(e);
            e.preventDefault();
            await this.handleEntryForm();
        });

        this.backBtn.addEventListener('click', () => {
            this.showEntryForm();
        });

        document.getElementById('clear-logs-btn').addEventListener('click', () => {
            this.clearLogs();
        });
    }

    async sendData(data) {
        const saveData = { ...data };

        try {
            const response = await fetch('/create/room', {
                method: "POST",
                body: JSON.stringify(saveData),
                headers: {
                    "Accept": "application/json",
                    "Content-Type": "application/json",
                }
            });

            if (response.status !== 200) {
                this.showAlert("failed to start conversion. Try again later...", "danger");
                throw new Error("failed to start conversation");
            }

            this.log(await response.json());
        } catch (err) {
            console.error(err);
            this.showAlert(err);
        }
    }

    async handleEntryForm() {
        const formData = new FormData(this.entryForm);
        const data = Object.fromEntries(formData);

        // Валидация
        if (!data.open_ai_token || !data.room_id || !data.peer_id || !data.lang) {
            this.showAlert('Пожалуйста, заполните все поля', 'danger');
            return;
        }

        console.log(data);

        await this.sendData(data);

        this.peerID = data.peer_id;
        this.roomID = data.room_id;
        this.lang = data.lang;
        this.showSessionForm(data);

        this.log('✅ Конфигурация сохранена');
        this.log(`👤 Пользователь: ${data.peer_id}`);
        this.log(`🏠 Комната: ${data.room_id}`);
        this.log(`🌐 Язык: ${data.lang === 'ru' ? 'Русский' : 'English'}`);
    }

    showEntryForm() {
        this.entryContainer.classList.remove('hidden');
        this.sessionContainer.classList.add('hidden');
    }

    log(message) {
        const timestamp = new Date().toLocaleTimeString();
        this.logs.innerHTML += `<div>[${timestamp}] ${message}</div>`;
        this.logs.scrollTop = this.logs.scrollHeight;
    }

    clearLogs() {
        this.logs.innerHTML = '';
    }

    showAlert(message, type) {
        // Создаем временный alert
        const alertDiv = document.createElement('div');
        alertDiv.className = `alert alert-${type} alert-dismissible fade show position-fixed`;
        alertDiv.style.cssText = 'top: 20px; right: 20px; z-index: 9999; max-width: 300px;';
        alertDiv.innerHTML = `
                    ${message}
                    <button type="button" class="btn-close" data-bs-dismiss="alert"></button>
                `;

        document.body.appendChild(alertDiv);

        // Автоматически убираем через 3 секунды
        setTimeout(() => {
            if (alertDiv.parentNode) {
                alertDiv.remove();
            }
        }, 3000);
    }

    showSessionForm(data) {
        this.entryContainer.classList.add('hidden');
        this.sessionContainer.classList.remove('hidden');

        this.userInfo.textContent = `${data.peer_id} @ ${data.room_id}`;

        // Инициализация WebRTC (заглушка)
        this.initializeWebRTC(data);
    }

    initializeWebRTC(config) {
        const startSessionBtn = document.getElementById('start-session-btn');
        const ws = new WebSocket(`ws://localhost:8081/subscribe?room_id=${this.roomID}&lang=${this.lang}&peer_id=${this.peerID}`);
        const pc = new RTCPeerConnection({
            iceServers: [
                {
                    urls: 'stun:stun.l.google.com:19302'
                }
            ]
        });

        startSessionBtn.addEventListener('click', () => {
            copySDP();
        });

        ws.addEventListener('open', function (event) {
            console.log('ws started...');
        });

        ws.addEventListener('message', function (event) {
            console.log(JSON.parse(event.data));
            const data = JSON.parse(event.data);
            if (data.answer) {
                document.getElementById('remoteSessionDescription').value = data.answer;
                pc.setRemoteDescription(JSON.parse(atob(data.answer)))
            }
        });

        const log = msg => {
            document.getElementById('logs').innerHTML += msg + '<br>'
        }

        navigator.mediaDevices.getUserMedia({ video: false, audio: {
                channelCount: 1,
                echoCancellation: false,
                noiseSuppression: false,
                autoGainControl: false,
                sampleRate: 48000 // hint
            } })
            .then(stream => {
                stream.getTracks().forEach(track => pc.addTrack(track, stream))
                pc.createOffer().then(d => pc.setLocalDescription(d)).catch(log)
            }).catch(log)

        pc.oniceconnectionstatechange = e => log(pc.iceConnectionState)
        pc.onicecandidate = event => {
            if (event.candidate === null) {
                document.getElementById('localSessionDescription').value = btoa(JSON.stringify(pc.localDescription))
                startSessionBtn.removeAttribute('disabled');
            }
        }

        pc.ontrack = event => {
            console.log(event);
            const el = document.createElement(event.track.kind)
            el.srcObject = event.streams[0]
            el.autoplay = true
            el.controls = true

            document.getElementById('audio1').appendChild(el)
        };

        window.copySDP = () => {
            const browserSDP = document.getElementById('localSessionDescription')

            browserSDP.focus()
            browserSDP.select()

            try {
                ws.send(JSON.stringify({
                    "message_type": "offer",
                    "message_data": browserSDP.value
                }));
                const successful = document.execCommand('copy')
                const msg = successful ? 'successful' : 'unsuccessful'
                log('Copying SDP was ' + msg)
            } catch (err) {
                log('Unable to copy SDP ' + err)
            }
        }
    }
}

new WebrtcApp();