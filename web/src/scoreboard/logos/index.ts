// Which logo the network diagram shows for a service, an operating system,
// and the scoring engine itself. See README.md for where each file is from.

import apache from './apache.svg';
import centos from './centos.svg';
import certificate from './certificate.svg';
import debian from './debian.svg';
import docker from './docker.svg';
import fedora from './fedora.svg';
import folderShare from './folder-share.svg';
import linux from './linux.svg';
import mail from './mail.svg';
import network from './network.svg';
import nginx from './nginx.svg';
import postgresql from './postgresql.svg';
import radar from './radar.svg';
import rockylinux from './rockylinux.svg';
import server from './server.svg';
import terminal from './terminal-2.svg';
import ubuntu from './ubuntu.svg';
import usersGroup from './users-group.svg';
import windows from './windows11.svg';
import wordpress from './wordpress.svg';
import worldWww from './world-www.svg';
import world from './world.svg';
import xmpp from './xmpp.svg';

/**
 * A logo's file, and whether it is single-colour: those are drawn in the
 * current text colour rather than as an image, so they read on any
 * background.
 */
export interface Logo {
  src: string;
  mono: boolean;
}

const brand = (src: string): Logo => ({ src, mono: false });
const mono = (src: string): Logo => ({ src, mono: true });

/**
 * Service logos by a word that may appear in the service's name, checked in
 * order, so "nginx-docker" is nginx and not docker.
 */
const byServiceName: [RegExp, Logo][] = [
  [/apache|httpd/, brand(apache)],
  [/nginx/, mono(nginx)],
  [/wordpress/, brand(wordpress)],
  [/docker/, brand(docker)],
  [/postgres/, brand(postgresql)],
  [/iis/, brand(windows)],
  [/openfire|xmpp|jabber|chat/, mono(xmpp)],
  [/smb|samba|cifs|share/, mono(folderShare)],
  [/smtp|mail|imap|pop3?/, mono(mail)],
  [/dns/, mono(worldWww)],
  [/\bad\b|ldap|active.?directory|\bdc\b/, mono(usersGroup)],
  [/\bca\b|cert|pki/, mono(certificate)],
  [/telnet|ssh|shell/, mono(terminal)],
];

/** The logo for a service whose name matches none of the above. */
const byCheckType: Record<string, Logo> = {
  http: mono(world),
  http_exclusive: mono(world),
  postgres: brand(postgresql),
  smb: mono(folderShare),
  smtp: mono(mail),
  dns: mono(worldWww),
  ldap: mono(usersGroup),
  openfire: mono(xmpp),
};

export function serviceLogo(name: string, checkType: string): Logo {
  const lower = name.toLowerCase();
  return byServiceName.find(([re]) => re.test(lower))?.[1] ?? byCheckType[checkType] ?? mono(network);
}

const byOS: Record<string, { logo: Logo; label: string }> = {
  windows: { logo: brand(windows), label: 'Windows' },
  linux: { logo: mono(linux), label: 'Linux' },
  ubuntu: { logo: brand(ubuntu), label: 'Ubuntu' },
  debian: { logo: brand(debian), label: 'Debian' },
  centos: { logo: brand(centos), label: 'CentOS' },
  rocky: { logo: brand(rockylinux), label: 'Rocky Linux' },
  fedora: { logo: brand(fedora), label: 'Fedora' },
};

/** The logo for a host's configured OS; a generic server when it has none we know. */
export function osLogo(os: string | undefined): Logo {
  return byOS[os?.toLowerCase() ?? '']?.logo ?? mono(server);
}

/** How to write a host's configured OS: properly capitalised if we know it, else as configured. */
export function osLabel(os: string | undefined): string | undefined {
  return byOS[os?.toLowerCase() ?? '']?.label ?? os;
}

export const engineLogo = mono(radar);
