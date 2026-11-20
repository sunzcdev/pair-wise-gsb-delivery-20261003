Name the layer a creator plugin failed at, and keep a plugin that cannot be imported from taking the whole run down.
A plugin whose dependency is missing is now logged and skipped, so ``--help`` still lists the plugins that loaded and
automatic selection falls back to a compatible creator. Naming the broken plugin with ``--creator`` reports the original
import error instead of silently using another implementation, and a plugin that loads but does not support the
interpreter reports that instead of a load error.
