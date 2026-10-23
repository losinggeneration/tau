// Original file: proto/plugin.proto

import type { ToolRegistration as _plugins_ToolRegistration, ToolRegistration__Output as _plugins_ToolRegistration__Output } from '../plugins/ToolRegistration.ts';
import type { CommandRegistration as _plugins_CommandRegistration, CommandRegistration__Output as _plugins_CommandRegistration__Output } from '../plugins/CommandRegistration.ts';
import type { CLIFlag as _plugins_CLIFlag, CLIFlag__Output as _plugins_CLIFlag__Output } from '../plugins/CLIFlag.ts';
import type { VirtualModelRegistration as _plugins_VirtualModelRegistration, VirtualModelRegistration__Output as _plugins_VirtualModelRegistration__Output } from '../plugins/VirtualModelRegistration.ts';

export interface Registrations {
  'tools'?: (_plugins_ToolRegistration)[];
  'commands'?: (_plugins_CommandRegistration)[];
  'eventSubscriptions'?: (string)[];
  'cliFlags'?: (_plugins_CLIFlag)[];
  'toolPrefix'?: (string);
  'virtualModels'?: (_plugins_VirtualModelRegistration)[];
}

export interface Registrations__Output {
  'tools'?: (_plugins_ToolRegistration__Output)[];
  'commands'?: (_plugins_CommandRegistration__Output)[];
  'eventSubscriptions'?: (string)[];
  'cliFlags'?: (_plugins_CLIFlag__Output)[];
  'toolPrefix'?: (string);
  'virtualModels'?: (_plugins_VirtualModelRegistration__Output)[];
}
