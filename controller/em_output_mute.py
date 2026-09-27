"""
Mute for the media player's OUTPUT, as Home Assistant means it: silence now,
and the same volume back on unmute.

We advertised VOLUME_MUTE to HA and then dropped the command as unhandled,
while reporting muted=False — so HA and Music Assistant both showed a mute
button that did nothing (#641). Music Assistant can fake a mute by setting 0
and restoring, but it uses the player's own mute when HA says there is one.

Done here rather than on the device so it works on every firmware in the
field: mute sends volume 0 and remembers the level; unmute sends it back.
Any other volume while muted — HA's slider, the Echo's buttons — unmutes at
that level, which is what a volume change on a muted player normally does.

This is not the Dot's mute button. That mutes the MICROPHONES, is sovereign
on the device, and is reported separately (#438).

Pure: the caller sends the levels it returns and persists only what it is
told to.
"""


class OutputMute:
    def __init__(self):
        self.muted = False
        self._restore: int | None = None   # level to put back on unmute

    def mute(self, current_level: int) -> int | None:
        """Level to send now, or None if already muted."""
        if self.muted:
            return None
        self.muted = True
        self._restore = int(current_level)
        return 0

    def unmute(self) -> int | None:
        """Level to send now, or None if not muted."""
        if not self.muted:
            return None
        self.muted = False
        level, self._restore = self._restore, None
        return level

    def volume_set(self, level: int) -> None:
        """An explicit volume from HA ends a mute."""
        self.muted = False
        self._restore = None

    def device_report(self, level: int) -> bool:
        """
        The device reported its level. True when it is a real volume to keep
        (persist as startupVolume and show HA); False when it is our own mute
        echoing back, which must not overwrite the level unmute restores.

        A non-zero level while muted is someone pressing the Echo's volume
        buttons, and that unmutes.
        """
        if not self.muted:
            return True
        if int(level) == 0:
            return False
        self.muted = False
        self._restore = None
        return True

    def on_reconnect(self) -> int | None:
        """Level to send after the device reconnects: it boots at its stored
        startupVolume, which is the pre-mute level, so a mute must be
        re-applied."""
        return 0 if self.muted else None

    @property
    def restore_level(self) -> int | None:
        return self._restore
