import { existsSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { cp } from 'node:fs/promises';
import type { ForgeConfig } from '@electron-forge/shared-types';
import { MakerZIP } from '@electron-forge/maker-zip';
import { MakerDeb } from '@electron-forge/maker-deb';
import { MakerRpm } from '@electron-forge/maker-rpm';
import { VitePlugin } from '@electron-forge/plugin-vite';
import { FusesPlugin } from '@electron-forge/plugin-fuses';
import { FuseV1Options, FuseVersion } from '@electron/fuses';

const runtimeDirectory = path.resolve('.runtime/runtime');
const bundled = existsSync(path.join(runtimeDirectory, 'manifest.json'));
if (process.env.SUCCESSBOX_RELEASE === '1' && !bundled)
  throw new Error('Release packaging requires the complete runtime bundle');
const dependencies =
  bundled && process.platform === 'linux'
    ? (JSON.parse(
        readFileSync(
          path.join(runtimeDirectory, 'linux-dependencies.json'),
          'utf8',
        ),
      ) as string[])
    : [];

const config: ForgeConfig = {
  packagerConfig: {
    asar: true,
    executableName: 'successbox',
    extraResource: ['assets', '../api/gateway/v1/gateway.proto'],
    afterCopyExtraResources: [
      async ({ buildPath, platform }) => {
        if (!bundled) return;
        // Packager's default fs.cp resolves symlinks into the build checkout.
        // Keep xrdp/FreeRDP library links and the Claude launcher relocatable.
        const resources =
          platform === 'darwin'
            ? path.join(buildPath, 'Successbox.app', 'Contents', 'Resources')
            : path.join(buildPath, 'resources');
        await cp(runtimeDirectory, path.join(resources, 'runtime'), {
          recursive: true,
          verbatimSymlinks: true,
        });
      },
    ],
  },
  rebuildConfig: {},
  makers: [
    new MakerZIP({}, ['darwin', 'linux']),
    // `bin` must match `executableName`; the makers otherwise look for the
    // package.json name.
    new MakerDeb({
      options: {
        name: 'successbox',
        bin: 'successbox',
        icon: 'assets/icon.png',
        categories: ['Development'],
        depends: [
          ...new Set([
            ...dependencies,
            'pkexec',
            'slirp4netns',
            'socat',
            'xauth',
            'python3',
            'openssl',
            'git',
            'openbox',
            'xterm',
            'dbus-x11',
            'tigervnc-standalone-server',
            'fonts-dejavu-core',
            'libx11-6',
            'libxext6',
            'libxinerama1',
            'libxcursor1',
            'libxfixes3',
            'libxrandr2',
            'libxrender1',
            'libxi6',
            'libxkbfile1',
            'libxv1',
            'libssl3',
          ]),
        ],
      },
    }),
    new MakerRpm({
      options: {
        name: 'successbox',
        bin: 'successbox',
        icon: 'assets/icon.png',
        categories: ['Development'],
      },
    }),
  ],
  plugins: [
    new VitePlugin({
      build: [
        {
          entry: 'src/main/main.ts',
          config: 'vite.main.config.mts',
          target: 'main',
        },
        {
          entry: 'src/preload/preload.ts',
          config: 'vite.preload.config.mts',
          target: 'preload',
        },
      ],
      renderer: [{ name: 'main_window', config: 'vite.renderer.config.mts' }],
    }),
    new FusesPlugin({
      version: FuseVersion.V1,
      [FuseV1Options.RunAsNode]: false,
      [FuseV1Options.EnableCookieEncryption]: true,
      [FuseV1Options.EnableNodeOptionsEnvironmentVariable]: false,
      [FuseV1Options.EnableNodeCliInspectArguments]: false,
      [FuseV1Options.EnableEmbeddedAsarIntegrityValidation]: true,
      [FuseV1Options.OnlyLoadAppFromAsar]: true,
    }),
  ],
};

export default config;
