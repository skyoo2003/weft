#!/usr/bin/env ruby
# frozen_string_literal: true

# GoReleaser currently emits the deprecated imperative `postflight` Cask DSL.
# Convert its quarantine hook to Homebrew's declarative install-steps DSL.
path = ARGV.fetch(0)
source = File.read(path)

legacy = /  postflight do\n    if OS\.mac\?\n(?<commands>(?:      system_command "\/usr\/bin\/xattr", args: \["-dr", "com\.apple\.quarantine", "#\{staged_path\}\/[^"]+"\]\n)+)    end\n  end/

if source.match?(legacy)
  source.sub!(legacy) do
    commands = Regexp.last_match(:commands)
    commands = commands.gsub(/^ {6}system_command/, "      run")
                       .gsub('#{staged_path}', "{{staged_path}}")
    "  postflight_steps do\n    on_macos do\n#{commands}    end\n  end"
  end
elsif !source.include?("postflight_steps do")
  abort "No supported GoReleaser postflight hook found in #{path}"
end

File.write(path, source)
