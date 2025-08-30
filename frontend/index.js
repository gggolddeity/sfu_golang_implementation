class WebrtcApp {
    constructor() {
        // entry
        this.entryForm = document.getElementById('container__entry__form');
        this.entryContainer = document.getElementById('entry-form-container');
        this.remoteAudioEls = new Map();

        this.makingOffer = false;
        this.isSettingRemoteAnswerPending = false;
        this.joinForm = document.getElementById('container__join__form');

        this.remoteAudioContainer = document.getElementById('remote-audio-container');
        if (!this.remoteAudioContainer) {
            this.remoteAudioContainer = document.createElement('div');
            this.remoteAudioContainer.id = 'remote-audio-container';
            this.remoteAudioContainer.style.cssText = 'position:absolute;left:-9999px;top:-9999px;height:0;width:0;overflow:hidden;';
            document.body.appendChild(this.remoteAudioContainer);
        }

        // session
        this.sessionContainer = document.getElementById('session-container');
        this.sessionContainer.addEventListener('click', () => {
            for (const el of this.remoteAudioEls.values()) {
                el.play().catch(()=>{});
            }
        }, { once: true });

        this.backBtn = document.getElementById('back-btn');
        this.sendMsgBtn = document.getElementById('send-msg-btn');
        this.chatInput = document.getElementById('chat-input');
        this.spinnerLoader = document.getElementById('spinner-loading');
        this.userInfo = document.getElementById('user-info');
        this.logs = document.getElementById('logs');

        // ui: peers & chat
        this.peersList = document.getElementById('peers-list');
        this.peersCount = document.getElementById('peers-count');
        this.chatTabs = document.getElementById('chat-tabs');
        this.chatTabContent = document.getElementById('chat-tab-content');
        this.activeLangs = document.getElementById('active-langs');
        this.muteBtn = document.getElementById('btn-mute');

        // audio
        this.remoteAudio = document.getElementById('remoteAudio');

        // state
        this.roomID = null;
        this.peerID = null;
        this.lang = null;
        this.ws = null;
        this.pc = null;
        this.localStream = null;
        this.micTrack = null;
        this.micSender = null;
        this.isMuted = false;
        this.isLoading = true;

        // maps for chat panes
        this.chatLangToPaneId = new Map();

        this.initEventListeners();
    }

    sendWS(message) {
        if (this.ws && this.ws.readyState === WebSocket.OPEN) {
            this.ws.send(message);
        } else {
            this.log('WS not open, drop send: ' + message);
        }
    }

    initEventListeners() {
        this.entryForm.addEventListener('submit', async (e) => {
            e.preventDefault();
            await this.handleEntryForm();
        });

        window.addEventListener("keydown",  (event) => {
            if (event.key === "Enter" && this?.chatInput?.value && this?.chatInput?.value !== "" && this.ws?.readyState === WebSocket.OPEN) {
                this.sendWS(
                    JSON.stringify({
                        message_type: 'message',
                        message_data: this.chatInput.value
                    })
                );
                this.chatInput.value = "";
            }
        });

        this.backBtn.addEventListener('click', async () => {
            await this.closeConnection();
            this.showEntryForm()
        });
        document.getElementById('clear-logs-btn').addEventListener('click', () => this.clearLogs());
        this.joinForm.addEventListener('submit', async (e) => {
            e.preventDefault();
            await this.handleJoinForm();
        });

        this.muteBtn.addEventListener('click', () => this.toggleMute());
        this.sendMsgBtn.addEventListener('click', () => {
            this.sendWS(
                JSON.stringify({
                    message_type: 'message',
                    message_data: this.chatInput.value
                })
            );
            this.chatInput.value = "";
        })
    }

    async closeConnection() {
        try {
            this.sendWS(JSON.stringify({
                message_type: 'leave',
                message_data: ""
            }));
            this.log('📨 Вышел из комнаты');
        } catch (err) {
            this.log('Ошибка отправки выхода: ' + err);
        }
    }

    async handleJoinForm() {
        const formData = new FormData(this.joinForm);
        const room_id = formData.get('room_id_join');
        const peer_id = formData.get('peer_id_join');
        const lang = formData.get('lang_join');

        if (!room_id || !peer_id || !lang) {
            this.showAlert('Укажите room_id, peer_id и язык', 'danger');
            return;
        }

        this.peerID = String(peer_id);
        this.roomID = String(room_id);
        this.lang = String(lang);

        await this.sendJoin({
            room_id: this.roomID,
            peer_id: this.peerID,
            lang: this.lang
        });

        this.showSessionForm({ peer_id: this.peerID, room_id: this.roomID });
        this.log('✅ Подключение к комнате (join)');
        this.log(`👤 ${this.peerID} @ 🏠 ${this.roomID} 🌐 ${this.lang}`);
    }

    async sendJoin(payload) {
        try {
            const response = await fetch('/join/room', {
                method: 'POST',
                body: JSON.stringify(payload),
                headers: { 'Accept': 'application/json', 'Content-Type': 'application/json' }
            });
            if (response.status !== 200) {
                this.showAlert('Не удалось войти в комнату (join).', 'danger');
                throw new Error('failed to join room');
            }
            this.log(JSON.stringify(await response.json()));
        } catch (err) {
            console.error(err);
            this.showAlert(err.message || String(err), 'danger');
        }
    }

    async handleEntryForm() {
        const formData = new FormData(this.entryForm);
        const data = Object.fromEntries(formData);

        if (!data.open_ai_token || !data.room_id || !data.peer_id || !data.lang) {
            this.showAlert('Пожалуйста, заполните все поля', 'danger');
            return;
        }

        // сохраняем в state
        this.peerID = data.peer_id;
        this.roomID = data.room_id;
        this.lang = data.lang;

        // отправляем конфиг на сервер (как было)
        await this.sendData({
            open_ai_token: data.open_ai_token,
            room_id: data.room_id,
            peer_id: data.peer_id,
            lang: data.lang
        });

        this.showSessionForm(data);

        this.log('✅ Конфигурация сохранена');
        this.log(`👤 Пользователь: ${data.peer_id}`);
        this.log(`🏠 Комната: ${data.room_id}`);
        this.log(`🌐 Язык: ${data.lang}`);
    }

    async sendData(payload) {
        try {
            const response = await fetch('/create/room', {
                method: 'POST',
                body: JSON.stringify(payload),
                headers: { 'Accept': 'application/json', 'Content-Type': 'application/json' }
            });
            if (response.status !== 200) {
                this.showAlert('Не удалось стартовать сессию. Попробуйте позже.', 'danger');
                throw new Error('failed to start conversation');
            }
            this.log(JSON.stringify(await response.json()));
        } catch (err) {
            console.error(err);
            this.showAlert(err.message || String(err), 'danger');
        }
    }

    showEntryForm() {
        this.entryContainer.classList.remove('hidden');
        this.sessionContainer.classList.add('hidden');

        // аккуратно закрыть ресурсы
        try { this.ws && this.ws.close(); } catch {}
        try { this.pc && this.pc.close(); } catch {}
        try { this.localStream && this.localStream.getTracks().forEach(t=>t.stop()); } catch {}
        this.logs.innerHTML = '';
        this.peersList.innerHTML = '';
        this.peersCount.textContent = '0';
        this.chatTabs.innerHTML = '';
        this.chatTabContent.innerHTML = '';
        this.activeLangs.innerHTML = '';
        this.chatLangToPaneId.clear();
        this.isMuted = false;
        this.muteBtn.className = 'btn btn-outline-danger';
        this.muteBtn.innerHTML = `<i class="bi bi-mic-mute"></i> Мьют`;
    }

    showSessionForm(data) {
        this.entryContainer.classList.add('hidden');
        this.sessionContainer.classList.remove('hidden');

        this.userInfo.textContent = `${data.peer_id} @ ${data.room_id}`;
        this.initializeWebRTC();
    }

    log(message) {
        const ts = new Date().toLocaleTimeString();
        this.logs.innerHTML += `<div>[${ts}] ${message}</div>`;
        this.logs.scrollTop = this.logs.scrollHeight;
    }

    clearLogs() { this.logs.innerHTML = ''; }

    showAlert(message, type = 'primary') {
        const alertDiv = document.createElement('div');
        alertDiv.className = `alert alert-${type} alert-dismissible fade show position-fixed`;
        alertDiv.style.cssText = 'top: 20px; right: 20px; z-index: 9999; max-width: 360px;';
        alertDiv.innerHTML = `${message}<button type="button" class="btn-close" data-bs-dismiss="alert"></button>`;
        document.body.appendChild(alertDiv);
        setTimeout(() => alertDiv.remove(), 3000);
    }

    waitForIceGatheringComplete(pc) {
        return new Promise(resolve => {
            if (pc.iceGatheringState === 'complete') return resolve();
            const onStateChange = () => {
                if (pc.iceGatheringState === 'complete') {
                    pc.removeEventListener('icegatheringstatechange', onStateChange);
                    resolve();
                }
            };
            pc.addEventListener('icegatheringstatechange', onStateChange);
        });
    }

    async initializeWebRTC() {
        let wsUrl;
        const res = localStorage.getItem('env');
        if (res === "dev") {
            wsUrl = `ws://localhost:8081/subscribe?room_id=${encodeURIComponent(this.roomID)}&lang=${encodeURIComponent(this.lang)}&peer_id=${encodeURIComponent(this.peerID)}`;
        } else {
            wsUrl = `wss://aigism.ru/subscribe?room_id=${encodeURIComponent(this.roomID)}&lang=${encodeURIComponent(this.lang)}&peer_id=${encodeURIComponent(this.peerID)}`;
        }

        this.ws = new WebSocket(wsUrl);
        this.ws.addEventListener('open', () => {
            this.log('WS: opened');
            this.startMediaAndOffer().catch(err => this.log('startMediaAndOffer error: ' + err));
        });
        this.ws.addEventListener('close', () => this.log('WS: closed'));
        this.ws.addEventListener('error', (e) => this.log('WS error: ' + (e?.message || 'unknown')));

        this.ws.addEventListener('message', async(event) => {
            let payload;
            try { payload = JSON.parse(event.data); } catch (_) { return; }

            // 2) Ответ SDP от сервера
            if (payload.answer) {
                if (this.pc.signalingState !== 'have-local-offer') {
                    console.warn('drop unexpected answer in state', this.pc.signalingState);
                    return;
                }
                const sdpJson = JSON.parse(atob(payload.answer));
                this.pc.setRemoteDescription(sdpJson).then(()=>{
                    this.log('Remote SDP applied');
                }).catch(err => this.log('setRemoteDescription error: ' + err));
                return;
            }

            if (payload.offer) {
                const remote = JSON.parse(atob(payload.offer));
                const offerCollision = this.makingOffer || this.pc.signalingState !== 'stable';

                // мы вежливые: если коллизия — откатываем свой оффер
                if (offerCollision) {
                    try { await this.pc.setLocalDescription({ type: 'rollback' }); }
                    catch (e) { this.log('rollback failed: ' + e); }
                }

                await this.pc.setRemoteDescription(remote);
                console.log('server offer applied');

                // 2) локальный answer
                const answer = await this.pc.createAnswer();
                this.isSettingRemoteAnswerPending = true;
                await this.pc.setLocalDescription(answer);
                this.isSettingRemoteAnswerPending = false;

                // 3) дождаться ICE complete, т.к. без trickle надо положить кандидаты в SDP
                await this.waitForIceGatheringComplete(this.pc);

                // 4) отправляем answer серверу (твоё ws-сообщение)
                this.sendWS(JSON.stringify({
                    message_type: 'server_answer',
                    message_data: btoa(JSON.stringify(this.pc.localDescription)),
                }));
                return;
            }

            // 3) Heartbeat с пирамии и языками
            if (payload.type === 'evt.heartbeat' && payload.data) {
                const peers = Array.isArray(payload.data.peers) ? payload.data.peers : [];
                const langs = Array.isArray(payload.data.langs) ? payload.data.langs : [];
                this.renderPeers(peers);
                this.ensureLangTabs(langs.length ? langs : [this.lang]);
                return;
            }

            // 4) Сообщения (MessageView-подобные)
            if (this.looksLikeMessage(payload)) {
                const msgView = this.normalizeMessage(payload);
                this.renderMessageToTabs(msgView);
                return;
            }

            // на всякий случай логируем остальное
            this.log('WS msg: ' + event.data);
        });
    }

    async startMediaAndOffer() {
        // 2) RTCPeerConnection
        this.makingOffer = true;
        this.pc = new RTCPeerConnection({
            iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
        });

        this.pc.oniceconnectionstatechange = () => this.log('ICE: ' + this.pc.iceConnectionState);
        this.pc.onconnectionstatechange = () => {
            this.log('PC: ' + this.pc.connectionState);
            if (this.pc.connectionState === "connected") {
                this.spinnerLoader.classList.add('hidden');
            }
        };

        this.pc.ontrack = (event) => {
            const stream = event.streams?.[0];
            if (!stream) return;

            const sid = stream.id;
            let el = this.remoteAudioEls.get(sid);
            if (!el) {
                el = document.createElement('audio');
                el.autoplay = true;
                el.playsInline = true;
                el.controls = false;
                el.muted = false;
                el.srcObject = stream;

                const p = el.play();
                if (p && typeof p.catch === 'function') {
                    p.catch(() => this.log('⚠️ Автовоспроизведение заблокировано — кликните в окно, чтобы включить звук.'));
                }

                stream.addEventListener('removetrack', () => {
                    if (stream.getAudioTracks().length === 0) {
                        try { el.pause(); } catch {}
                        el.srcObject = null;
                        el.remove();
                        this.remoteAudioEls.delete(sid);
                    }
                });

                this.remoteAudioContainer.appendChild(el);
                this.remoteAudioEls.set(sid, el);
            } else {
                el.srcObject = stream;
                el.play().catch(()=>{});
            }
        };

        this.localStream = await navigator.mediaDevices.getUserMedia({
            video: false,
            audio: {
                channelCount: 2,
                echoCancellation: true,
                noiseSuppression: true,
                autoGainControl: false,
                sampleRate: 48000
            }
        });

        this.localStream.getTracks().forEach(t => this.pc.addTrack(t, this.localStream));
        this.micTrack = this.localStream.getAudioTracks()[0];
        this.micSender = this.pc.getSenders().find(s => s.track && s.track.kind === 'audio');

        const offer = await this.pc.createOffer();
        await this.pc.setLocalDescription(offer);
        this.makingOffer = false;
        await this.waitForIceGatheringComplete(this.pc);
        const ld = this.pc.localDescription;
        if (ld?.type === 'offer') {
            this.sendWS(JSON.stringify({ message_type: 'offer', message_data: btoa(JSON.stringify(ld))}));
            this.log('📨 Оффер отправлен (once)');
        }
    }

    /* ========== UI: Peers ========== */
    renderPeers(peers) {
        this.peersList.innerHTML = '';
        this.peersCount.textContent = String(peers.length);

        peers.forEach(p => {
            const initials = (p.name || p.id || '?').slice(0, 2).toUpperCase();
            const kindBadge = this.kindBadge(p.kind);
            const lang = p.lang ? `<span class="badge text-bg-light">${p.lang}</span>` : '';
            const selfMark = (p.id === this.peerID) ? `<span class="badge text-bg-primary ms-1">you</span>` : '';

            const div = document.createElement('div');
            div.className = 'peer-item';
            div.innerHTML = `
        <div class="peer-avatar">${initials}</div>
        <div class="peer-meta">
          <div><strong>${p.name || p.id || 'unknown'}</strong> ${selfMark}</div>
          <small>${kindBadge} ${lang}</small>
        </div>
      `;
            this.peersList.appendChild(div);
        });
    }

    kindBadge(kind) {
        let icon = 'bi-robot';
        if (kind === 'human') icon = 'bi-person';
        if (kind === 'sys') icon = 'bi-cpu';
        return `<span class="badge text-bg-secondary badge-kind"><i class="bi ${icon} me-1"></i>${(kind||'').toUpperCase()}</span>`;
    }

    /* ========== UI: Chat Tabs ========== */
    ensureLangTabs(langs) {
        // создать вкладки, если их нет
        langs.forEach(l => {
            if (this.chatLangToPaneId.has(l)) return;

            const tabId = `chat-tab-${l}`;
            this.chatLangToPaneId.set(l, tabId);

            // вкладка
            const li = document.createElement('li');
            li.className = 'nav-item';
            li.innerHTML = `
        <button class="nav-link ${l === this.lang ? 'active' : ''}" id="tab-btn-${l}" data-bs-toggle="tab" data-bs-target="#${tabId}" type="button" role="tab" aria-controls="${tabId}" aria-selected="${l===this.lang}">
          ${this.langFlag(l)} ${l.toUpperCase()}
        </button>`;
            this.chatTabs.appendChild(li);

            // панель
            const pane = document.createElement('div');
            pane.className = `tab-pane fade ${l === this.lang ? 'show active' : ''}`;
            pane.id = tabId;
            pane.setAttribute('role', 'tabpanel');
            pane.innerHTML = `<div class="chat-messages" id="chat-messages-${l}"></div>`;
            this.chatTabContent.appendChild(pane);
        });

        // бейджи активных языков сверху
        this.activeLangs.innerHTML = '';
        langs.forEach(l => {
            const b = document.createElement('span');
            b.className = 'badge text-bg-light';
            b.textContent = l;
            this.activeLangs.appendChild(b);
        });
    }

    langFlag(l) {
        if (l === 'ru') return '🇷🇺';
        if (l === 'en') return '🇺🇸';
        return '🏳️';
    }

    appendToLang(lang, html) {
        // если прилетел новый язык — создаём вкладку
        if (!this.chatLangToPaneId.has(lang)) {
            this.ensureLangTabs([lang]);
        }
        const box = document.getElementById(`chat-messages-${lang}`);
        if (!box) return;
        box.insertAdjacentHTML('beforeend', html);
        box.scrollTop = box.scrollHeight;
    }

    /* ========== Messages handling ========== */
    looksLikeMessage(payload) {
        // гибко: или payload.type начинается с evt.message, или есть поле msg как map
        if (payload?.type && String(payload.type).startsWith('evt.message')) return true;
        if (payload?.data && payload.data.msg) return true;
        if (payload?.msg) return true;
        return false;
    }

    normalizeMessage(payload) {
        // пробуем привести к виду { from:{name,id,kind,lang}, msg:{lang:text}, at, seq }
        const d = payload.data ?? payload;
        const from = d.from || d.from_id || d.From || {};
        const peerKind = d.peer_kind || d.PeerKind || from.kind || 'human';
        const when = d.at || d.At || new Date().toISOString();
        const seq = d.msgSeq || d.MsgSeq || d.seq || 0;

        // msg-поле: может быть строка или объект MsgPart
        const msgMap = d.msg || d.Msg || {};
        const normalized = {};
        Object.keys(msgMap).forEach(l => {
            const v = msgMap[l];
            if (typeof v === 'string') normalized[l] = v;
            else if (v && typeof v === 'object') normalized[l] = v.text || v.content || v.value || JSON.stringify(v);
            else normalized[l] = String(v);
        });

        return {
            from: {
                id: from.id || from.ID || 'unknown',
                name: from.name || from.Name || from || 'unknown',
                kind: peerKind,
                lang: from.lang || from.Lang
            },
            msg: normalized,
            at: when,
            seq
        };
    }

    renderMessageToTabs(msgView) {
        const ts = new Date(msgView.at).toLocaleTimeString();
        const author = msgView.from.name || msgView.from.id || 'unknown';
        const kindIcon = (msgView.from.kind === 'bot') ? 'bi-robot' :
            (msgView.from.kind === 'sys') ? 'bi-cpu' : 'bi-person';

        Object.entries(msgView.msg).forEach(([lang, text]) => {
            const html = `
        <div class="mb-2">
          <div class="d-flex align-items-center gap-2">
            <span class="badge text-bg-secondary"><i class="bi ${kindIcon} me-1"></i>${author}</span>
            <small class="text-muted">${ts}</small>
          </div>
          <div class="mt-1">${this.escapeHtml(text).replace(/\n/g,'<br>')}</div>
        </div>`;
            this.appendToLang(lang, html);
        });
    }

    escapeHtml(s) {
        return String(s)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;');
    }

    /* ========== Mute ========== */
    toggleMute() {
        if (!this.micTrack) return;
        this.isMuted = !this.isMuted;
        // мягкий мьют
        this.micTrack.enabled = !this.isMuted;

        // UI
        if (this.isMuted) {
            this.muteBtn.classList.remove('btn-outline-danger');
            this.muteBtn.classList.add('btn-danger');
            this.muteBtn.innerHTML = `<i class="bi bi-mic-mute"></i> Размьют`;
        } else {
            this.muteBtn.classList.add('btn-outline-danger');
            this.muteBtn.classList.remove('btn-danger');
            this.muteBtn.innerHTML = `<i class="bi bi-mic"></i> Мьют`;
        }
    }
}

new WebrtcApp();