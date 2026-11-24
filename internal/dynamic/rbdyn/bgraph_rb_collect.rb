# bgraph's Ruby collector (docs/dynamic-layer.md section 10.4). `bgraph record --eco ruby` runs every test file in a process
# tree of its own with RUBYOPT=-rbgraph_rb_collect (this file's directory on RUBYLIB), so it loads first in every Ruby process
# of the tree. It starts Ruby's Coverage in methods mode before anything else loads, and observes, for files under the
# repository root ($BGRAPH_RB_ROOT): files read as data, directories listed, the processes started, and anything that makes
# the record incomplete. At exit (its at_exit handler is registered first, so it runs last) it writes
# $BGRAPH_RB_OUT/<pid>-<random>.json. bgraph merges the files of a unit (internal/dynamic/rbdyn).
#
# It must not load any gem: a library loaded here would be activated before Bundler resolves the bundle.
if ENV["BGRAPH_RB_OUT"] && !ENV["BGRAPH_RB_OUT"].empty? && !defined?(::BgraphRbCollect)
  require "rbconfig"

  module BgraphRbCollect
    ID = "ruby-cov-2"
    OUT = ENV["BGRAPH_RB_OUT"]
    WIN = (RUBY_PLATFORM =~ /mswin|mingw|cygwin/) ? true : false
    ROOT = ENV["BGRAPH_RB_ROOT"].to_s.tr("\\", "/").chomp("/")
    ROOT_KEY = (WIN ? ROOT.downcase : ROOT) + "/"
    # The root may be reached through a symbolic link (macOS /var -> /private/var): loaded files can carry either spelling.
    ROOT_KEYS = [ROOT_KEY, ((File.realpath(ROOT).tr("\\", "/").chomp("/") rescue ROOT)).then { |r| (WIN ? r.downcase : r) + "/" }].uniq.freeze
    SELF = File.expand_path(__FILE__).tr("\\", "/")
    LIBDIR = (defined?(RbConfig) ? RbConfig::CONFIG["rubylibdir"].to_s : "").tr("\\", "/")
    # Frames of these libraries list directories to decide what exists (loading machinery, test frameworks): not a dependency.
    EXEMPT_GEMS = %r{/gems/(?:zeitwerk|rspec-core|rspec-support|minitest|test-unit|rake|bundler|rubygems-update|bootsnap)-\d[^/]*/}
    RUBY_PROGS = %w[ruby jruby truffleruby bundle bundler rake rspec gem irb].freeze

    @incomplete = []
    @reads = {}
    @dirs = {}
    @children = []
    @own = false
    @pid = Process.pid
    @seq = 0

    class << self
      attr_reader :own

      def incomplete(why)
        @incomplete << why unless @incomplete.include?(why) || @incomplete.size > 50
      end

      def guard
        return if Thread.current[:__bgraph_rb_in]
        Thread.current[:__bgraph_rb_in] = true
        begin
          yield
        rescue Exception => e # rubocop:disable Lint/RescueException
          incomplete("the collector raised #{e.class}: #{e.message.to_s[0, 200]}")
        ensure
          Thread.current[:__bgraph_rb_in] = false
        end
      end

      # repo_path returns the absolute path (forward slashes) of a path argument when it lies under the repository root.
      def repo_path(p, base = nil)
        s = p.respond_to?(:to_path) ? p.to_path : p
        return nil unless s.is_a?(String)
        return nil if s.start_with?("|")
        abs = File.expand_path(s, base || Dir.pwd).tr("\\", "/")
        k = WIN ? abs.downcase : abs
        return nil unless ROOT_KEYS.any? { |rk| (k + "/") == rk || k.start_with?(rk) }
        abs
      rescue ArgumentError, TypeError, SystemCallError
        nil
      end

      def read(p)
        guard do
          if p.is_a?(String) && p.start_with?("|")
            child([p[1..]], {}, nil)
          elsif (abs = repo_path(p))
            @reads[abs] = true
          end
        end
      end

      def open_file(p, mode, opts)
        guard do
          mode = opts[:mode] || opts["mode"] || mode
          flags = opts[:flags]
          write_only = case mode
                       when String then (mode.start_with?("w") || (mode.start_with?("a") && !mode.include?("+")))
                       when Integer then (mode & 3) == File::WRONLY || (mode & File::TRUNC) != 0
                       else false
                       end
          write_only ||= flags.is_a?(Integer) && ((flags & 3) == File::WRONLY || (flags & File::TRUNC) != 0)
          if !write_only && (abs = repo_path(p))
            @reads[abs] = true
          end
        end
      end

      def stdlib?(p)
        !LIBDIR.empty? && p.start_with?(LIBDIR + "/")
      end

      # exempt? reports whether the hooked call comes from the loading machinery or a test framework (or a gemspec), looking
      # through the standard library's own frames (pathname, find, fileutils...) to their caller.
      def exempt?
        (caller_locations(1, 80) || []).each do |l|
          p = (l.absolute_path || l.path).to_s.tr("\\", "/")
          next if p == SELF || p.start_with?("<internal:")
          return true if p.end_with?(".gemspec")
          k = WIN ? p.downcase : p
          return false if ROOT_KEYS.any? { |rk| k.start_with?(rk) }
          return true if p =~ EXEMPT_GEMS
          return true if !LIBDIR.empty? && (p.start_with?(LIBDIR + "/rubygems") || p.start_with?(LIBDIR + "/bundler"))
          next if stdlib?(p)
          return false
        end
        false
      end

      def list(dir, base = nil)
        guard do
          abs = repo_path(dir, base)
          @dirs[abs] = true if abs && !exempt?
        end
      end

      # glob records the directory part of each glob pattern before its first wildcard (a pattern without one is an existence
      # check, not a listing).
      def glob(patterns, base)
        guard do
          Array(patterns).each do |pat|
            pat = pat.to_path if pat.respond_to?(:to_path)
            next unless pat.is_a?(String)
            parts = pat.tr("\\", "/").split("/", -1)
            i = parts.index { |s| s =~ /[*?\[{]/ }
            next if i.nil? # a literal path: an existence check, not a listing
            dir = parts[0...i].join("/")
            dir = "/" if dir.empty? && pat.start_with?("/")
            dir = "." if dir.empty?
            abs = repo_path(dir, base.nil? ? nil : (base.respond_to?(:to_path) ? base.to_path : base.to_s))
            @dirs[abs] = true if abs && !exempt?
          end
        end
      end

      # ruby_prog? reports whether a command's program runs Ruby (and therefore loads this collector through RUBYOPT).
      def ruby_prog?(prog)
        return false unless prog.is_a?(String) && !prog.empty?
        return true if prog == RbConfig.ruby || prog.tr("\\", "/") == RbConfig.ruby.tr("\\", "/")
        base = File.basename(prog.tr("\\", "/")).downcase.sub(/\.(exe|bat|cmd)\z/, "")
        return true if RUBY_PROGS.include?(base)
        file = File.expand_path(prog)
        if File.file?(file)
          line = File.open(file, "rb") { |f| f.gets.to_s } rescue ""
          return true if line.start_with?("#!") && line.include?("ruby")
        end
        false
      end

      SHELL_META = /[|&;<>()$`\n\\"'*?\[\]{}~]/

      # describe returns [program, ruby?] for the arguments of system / spawn / exec / popen.
      def describe(args)
        a = args.dup
        a.shift if a.first.is_a?(Hash)
        a.pop if a.last.is_a?(Hash)
        first = a.first
        if first.is_a?(Array)
          prog = first[0].to_s
          return [prog, ruby_prog?(prog)]
        end
        if a.size == 1
          cmd = first.to_s.strip
          if cmd =~ SHELL_META
            words = cmd.split(/\s+/)
            return ["shell: " + cmd[0, 80], false] unless words.size >= 1 && words[0] !~ SHELL_META && ruby_prog?(words[0]) &&
                                                         cmd !~ /[|&;<>()$`\n]/
            return [words[0], true]
          end
          prog = cmd.split(/\s+/).first.to_s
          return [prog, ruby_prog?(prog)]
        end
        prog = first.to_s
        [prog, ruby_prog?(prog)]
      end

      def child(args, _opts, pid, exec: false, fork: false)
        guard do
          if fork
            @children << { "pid" => pid.to_i, "ruby" => true, "cmd" => "fork" }
            next
          end
          next if exempt?
          prog, ruby = describe(args)
          @children << { "pid" => pid.to_i, "ruby" => ruby, "cmd" => prog.to_s[0, 120], "exec" => exec }
        end
      end

      def last_pid(st)
        st.respond_to?(:pid) ? st.pid : 0
      end

      def json(v)
        case v
        when Hash then "{" + v.map { |k, x| json(k.to_s) + ":" + json(x) }.join(",") + "}"
        when Array then "[" + v.map { |x| json(x) }.join(",") + "]"
        when String
          s = v.dup.force_encoding(Encoding::UTF_8)
          s = s.encode(Encoding::UTF_8, invalid: :replace, undef: :replace, replace: "?") unless s.valid_encoding?
          '"' + s.gsub(/["\\\u0000-\u001f]/) { |c| format("\\u%04x", c.ord) } + '"'
        when Integer then v.to_s
        when true then "true"
        when false then "false"
        when nil then "null"
        else json(v.to_s)
        end
      end

      def under_root(p)
        k = (WIN ? p.downcase : p)
        ROOT_KEYS.any? { |rk| k.start_with?(rk) }
      end

      # dump writes what this process saw. exec: written right before exec replaces the process.
      def dump(exec: false)
        @own = true
        methods = {}
        begin
          cov = Coverage.peek_result
          cov.each do |file, data|
            f = file.to_s.tr("\\", "/")
            next unless under_root(f)
            ms = data.is_a?(Hash) ? data[:methods] : nil
            if ms.nil?
              incomplete("Coverage has no method table (another Coverage mode was set up first)")
              next
            end
            methods[f] = ms.map { |key, count| [key[2], key[4], count] }
          end
        rescue StandardError => e
          incomplete("Coverage is not running at exit (#{e.class}): the tests stopped it")
        end
        loaded = $LOADED_FEATURES.map { |f| f.to_s.tr("\\", "/") }.select { |f| under_root(f) }
        @seq += 1
        out = {
          "collector" => ID, "pid" => Process.pid, "ppid" => (Process.ppid rescue 0), "exec" => exec,
          "methods" => methods, "loaded" => loaded, "reads" => @reads.keys, "dirs" => @dirs.keys,
          "children" => @children, "incomplete" => @incomplete
        }
        name = File.join(OUT, "#{Process.pid}-#{@seq}-#{rand(1 << 30)}.json")
        File.binwrite(name + ".tmp", json(out))
        File.rename(name + ".tmp", name)
      rescue Exception => e # rubocop:disable Lint/RescueException
        begin
          File.binwrite(File.join(OUT, "#{Process.pid}-error.json"),
                        json({ "collector" => ID, "pid" => Process.pid, "ppid" => 0, "exec" => exec, "methods" => {}, "loaded" => [],
                               "reads" => [], "dirs" => [], "children" => [],
                               "incomplete" => ["the collector could not write its output: #{e.class}: #{e.message.to_s[0, 200]}"] }))
        rescue Exception # rubocop:disable Lint/RescueException
          nil
        end
      ensure
        @own = false
      end
    end

    require "coverage"
    if Coverage.respond_to?(:running?) && Coverage.running?
      incomplete("Coverage was already running when the collector loaded")
    elsif Coverage.respond_to?(:setup)
      Coverage.setup(methods: true, lines: true)
      Coverage.resume
    else
      Coverage.start(methods: true, lines: true)
    end

    module CoverageHook
      %i[setup start resume suspend result].each do |m|
        define_method(m) do |*a, **k, &b|
          BgraphRbCollect.incomplete("the tests called Coverage.#{m}") unless BgraphRbCollect.own
          super(*a, **k, &b)
        end
      end
    end
    Coverage.singleton_class.prepend(CoverageHook)

    module IOClassHook
      def read(name, *a, **k, &b)
        BgraphRbCollect.read(name)
        super
      end

      def binread(name, *a, **k, &b)
        BgraphRbCollect.read(name)
        super
      end

      def readlines(name = nil, *a, **k, &b)
        BgraphRbCollect.read(name)
        super
      end

      def foreach(name, *a, **k, &b)
        BgraphRbCollect.read(name)
        super
      end

      def copy_stream(src, *a, **k, &b)
        BgraphRbCollect.read(src) if src.is_a?(String) || src.respond_to?(:to_path)
        super
      end

      def popen(*a, **k, &b)
        info = BgraphRbCollect
        if b
          super(*a, **k) do |io|
            info.child(a, k, io.pid)
            b.call(io)
          end
        else
          io = super(*a, **k)
          info.child(a, k, io.respond_to?(:pid) ? io.pid : 0)
          io
        end
      end
    end
    IO.singleton_class.prepend(IOClassHook)

    module FileHook
      def initialize(name, *a, **k, &b)
        BgraphRbCollect.open_file(name, a[0], k) unless name.is_a?(Integer)
        super
      end
    end
    File.prepend(FileHook)

    module DirClassHook
      def glob(pattern, *a, **k, &b)
        BgraphRbCollect.glob(pattern, k[:base])
        super
      end

      def [](*patterns, **k)
        BgraphRbCollect.glob(patterns, k[:base])
        super
      end

      %i[children entries each_child foreach].each do |m|
        define_method(m) do |dir, *a, **k, &b|
          BgraphRbCollect.list(dir)
          super(dir, *a, **k, &b)
        end
      end
    end
    Dir.singleton_class.prepend(DirClassHook)

    module DirHook
      def initialize(dir, *a, **k, &b)
        BgraphRbCollect.list(dir)
        super
      end
    end
    Dir.prepend(DirHook)

    module KernelHook
      def system(*a, **k, &b)
        r = super
        BgraphRbCollect.child(a, k, BgraphRbCollect.last_pid($?))
        r
      end

      def `(cmd)
        r = super
        BgraphRbCollect.child([cmd], {}, BgraphRbCollect.last_pid($?))
        r
      end

      def spawn(*a, **k, &b)
        pid = super
        BgraphRbCollect.child(a, k, pid)
        pid
      end

      def exec(*a, **k, &b)
        BgraphRbCollect.child(a, k, Process.pid, exec: true)
        BgraphRbCollect.dump(exec: true)
        super
      end

      def open(name, *a, **k, &b)
        if name.is_a?(String) && name.start_with?("|")
          BgraphRbCollect.child([name[1..]], {}, 0)
        end
        super
      end
    end
    Kernel.prepend(KernelHook)
    Kernel.singleton_class.prepend(KernelHook)

    module ProcessHook
      def spawn(*a, **k, &b)
        pid = super
        BgraphRbCollect.child(a, k, pid)
        pid
      end

      def exec(*a, **k, &b)
        BgraphRbCollect.child(a, k, Process.pid, exec: true)
        BgraphRbCollect.dump(exec: true)
        super
      end

      if Process.respond_to?(:_fork)
        def _fork(*a, **k, &b)
          pid = super
          BgraphRbCollect.child([], {}, pid, fork: true) if pid.to_i.positive?
          pid
        end
      end
    end
    Process.singleton_class.prepend(ProcessHook)

    unless Process.respond_to?(:_fork)
      module ForkHook
        def fork(*a, **k, &b)
          BgraphRbCollect.incomplete("fork without Process._fork (Ruby < 3.1): forked children are not tracked")
          super
        end
      end
      Kernel.prepend(ForkHook)
      Process.singleton_class.prepend(ForkHook)
    end

    at_exit { BgraphRbCollect.dump }
  end
end
