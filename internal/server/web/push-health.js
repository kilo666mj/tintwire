"use strict";

// App-owned delivery testing. Subscription lifecycle lives in pwa-kit.
(function (root) {
  function create(options) {
    const {testButton, repairButton, confirmButton, status, confirmation} = options;
    let state = {status: "off"}, userID = "", fingerprint = "", generation = 0, epoch = 0;
    let record = {}, busy = false, verifying = false, message = "";
    const now = options.now || Date.now;
    let storage = options.storage;
    try { storage ||= root.localStorage; } catch (_) {}
    const storageKey = () => `tintwire-push-test:${userID}`;
    function save() {
      try { storage.setItem(storageKey(), JSON.stringify(record)); } catch (_) {}
    }
    function reset() {
      epoch++;
      record = {};
      message = "";
      try { storage.removeItem(storageKey()); } catch (_) {}
      render();
    }
    function pending() {
      return record.fingerprint === fingerprint && record.pendingAt > 0 && now() - record.pendingAt < 10 * 60 * 1000;
    }
    function render() {
      const subscribed = state.status === "on" && Boolean(fingerprint);
      testButton.disabled = busy || verifying || !subscribed;
      repairButton.disabled = busy || !["on", "error"].includes(state.status);
      confirmButton.hidden = !subscribed || !pending();
      confirmButton.disabled = busy || verifying;
      status.textContent = message || (subscribed ? "Send a test notification to this device to check delivery." : "Subscribe on this device before testing delivery.");
      confirmation.textContent = record.fingerprint === fingerprint && record.confirmedAt
        ? `Last test confirmed by you: ${new Date(record.confirmedAt).toLocaleString()}.`
        : "No confirmed test for this subscription.";
    }
    async function setState(value, nextUserID) {
      const sequence = ++generation;
      if (userID !== nextUserID) {
        epoch++; fingerprint = ""; record = {}; message = "";
      }
      userID = nextUserID;
      state = value;
      verifying = value.status === "on";
      if (["off", "blocked", "signedOut", "repairing", "disabling"].includes(value.status)) reset();
      render();
      if (value.status !== "on" || !value.subscription) return;
      const serialized = value.subscription.toJSON();
      // Bind confirmation to endpoint AND keys. Never store the endpoint or keys.
      const identity = JSON.stringify([nextUserID, serialized.endpoint, serialized.keys]);
      let hash;
      try {
        hash = Array.from(new Uint8Array(await root.crypto.subtle.digest("SHA-256", new TextEncoder().encode(identity))), byte => byte.toString(16).padStart(2, "0")).join("");
      } catch (_) {
        if (sequence !== generation) return;
        verifying = false;
        message = "Device verification is unavailable. Reload and try again.";
        fingerprint = ""; render(); return;
      }
      if (sequence !== generation) return;
      verifying = false;
      if (fingerprint !== hash) {
        epoch++; fingerprint = hash; record = {}; message = "";
        try {
          const saved = JSON.parse(storage.getItem(storageKey()) || "{}");
          if (saved.fingerprint === hash) record = saved;
        } catch (_) {}
      }
      if (record.fingerprint !== hash) { record = {fingerprint: hash}; save(); }
      render();
    }
    async function testDevice() {
      if (testButton.disabled) return;
      busy = true; message = "Sending a test to this device…";
      delete record.pendingAt; save(); render();
      const activeEpoch = epoch;
      const subscription = state.subscription;
      try {
        const response = await (options.fetch || root.fetch)("/api/v1/push/test", {
          method: "POST", headers: {"Content-Type": "application/json"},
          body: JSON.stringify({endpoint: subscription.endpoint})
        });
        let result = {};
        try { result = await response.json(); } catch (_) {}
        if (activeEpoch !== epoch) return;
        if (!response.ok || result.outcome !== "provider_accepted") {
          throw new Error(result.message || (response.status === 404 || response.status === 410
            ? "This subscription is no longer registered. Repair alerts and test again."
            : response.status === 429 ? "Wait a minute before testing again." : "The test could not be sent. Try again."));
        }
        record.pendingAt = now(); save();
        message = "The push service accepted the test. Did it appear on this device? If not, try Repair alerts.";
      } catch (error) {
        if (activeEpoch === epoch) message = error.message || "The test could not be sent.";
      } finally { busy = false; render(); }
    }
    async function repair() {
      if (repairButton.disabled) return;
      const repairingUser = userID;
      reset(); busy = true; message = "Repairing alerts…"; render();
      try {
        const result = await options.client().repair();
        if (options.userID() !== repairingUser) return;
        // setState may still be hashing the new subscription; wait for it here.
        await setState(result, options.userID());
        message = result.status === "on" ? "Subscription renewed. Test this device to confirm delivery." : result.message;
      } catch (_) { message = "Unable to repair alerts. Try again."; }
      finally { busy = false; render(); }
    }
    function confirm() {
      if (busy || verifying || state.status !== "on" || !pending()) { render(); return; }
      record.confirmedAt = now(); delete record.pendingAt; save();
      message = "You confirmed seeing the test on this device. Future delivery can still be affected by device settings or connectivity.";
      render();
    }
    testButton.addEventListener("click", testDevice);
    repairButton.addEventListener("click", repair);
    confirmButton.addEventListener("click", confirm);
    render();
    return {setState, reset, refresh: render};
  }
  root.TintwirePushHealth = {create};
})(globalThis);
